package release

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/weaveworks/flux"
	"github.com/weaveworks/flux/cluster"
	"github.com/weaveworks/flux/git"
	"github.com/weaveworks/flux/policy"
	"github.com/weaveworks/flux/registry"
	"github.com/weaveworks/flux/update"
)

const (
	Locked         = "locked"
	NotIncluded    = "not included"
	Excluded       = "excluded"
	DifferentImage = "a different image"
	NotInCluster   = "not running in cluster"
	ImageNotFound  = "cannot find one or more images"
	ImageUpToDate  = "image(s) up to date"
)

type ReleaseContext struct {
	Cluster  cluster.Cluster
	Repo     *git.Checkout
	Registry registry.Registry
}

func NewReleaseContext(c cluster.Cluster, reg registry.Registry, repo *git.Checkout) *ReleaseContext {
	return &ReleaseContext{
		Cluster:  c,
		Repo:     repo,
		Registry: reg,
	}
}

func (rc *ReleaseContext) CommitAndPush(msg string, spec *update.ReleaseSpec, result update.Result) error {
	return rc.Repo.CommitAndPush(msg, &git.Note{
		JobID: "", // FIXME: get the job id here
		Spec: update.Spec{
			Type: update.Images,
			Spec: *spec,
		},
		Result: result,
	})
}

func (rc *ReleaseContext) PushChanges(updates []*ServiceUpdate, spec *update.ReleaseSpec, results update.Result) error {
	rc.Repo.Lock()
	err := writeUpdates(updates)
	if err != nil {
		rc.Repo.Unlock()
		return err
	}
	rc.Repo.Unlock()

	commitMsg := commitMessageFromReleaseSpec(spec)
	return rc.CommitAndPush(commitMsg, spec, results)
}

// Return the revision of HEAD as a commit hash.
func (rc *ReleaseContext) HeadRevision() (string, error) {
	return rc.Repo.HeadRevision()
}

func (rc *ReleaseContext) ListRevisions(ref string) ([]string, error) {
	return rc.Repo.RevisionsBetween(rc.Repo.SyncTag, ref)
}

func (rc *ReleaseContext) UpdateTag() error {
	return rc.Repo.MoveTagAndPush("HEAD", "Sync pointer")
}

// ---

func writeUpdates(updates []*ServiceUpdate) error {
	for _, update := range updates {
		fi, err := os.Stat(update.ManifestPath)
		if err != nil {
			return err
		}
		if err = ioutil.WriteFile(update.ManifestPath, update.ManifestBytes, fi.Mode()); err != nil {
			return err
		}
	}
	return nil
}

// SelectServices finds the services that exist both in the definition
// files and the running platform.
//
// `ServiceFilter`s can be provided to filter the found services.
// Be careful about the ordering of the filters. Filters that are earlier
// in the slice will have higher priority (they are run first).
func (rc *ReleaseContext) SelectServices(results update.Result, filters ...ServiceFilter) ([]*ServiceUpdate, error) {
	defined, err := rc.FindDefinedServices()
	if err != nil {
		return nil, err
	}

	var ids []flux.ServiceID
	definedMap := map[flux.ServiceID]*ServiceUpdate{}
	for _, s := range defined {
		ids = append(ids, s.ServiceID)
		definedMap[s.ServiceID] = s
	}

	// Correlate with services in running system.
	services, err := rc.Cluster.SomeServices(ids)
	if err != nil {
		return nil, err
	}

	// Compare defined vs running
	var updates []*ServiceUpdate
	for _, s := range services {
		update, ok := definedMap[s.ID]
		if !ok {
			// Found running service, but not defined...
			continue
		}
		update.Service = s
		updates = append(updates, update)
		delete(definedMap, s.ID)
	}

	// Filter both updates ...
	var filteredUpdates []*ServiceUpdate
	for _, s := range updates {
		fr := s.filter(filters...)
		results[s.ServiceID] = fr
		if fr.Status == update.ReleaseStatusSuccess || fr.Status == "" {
			filteredUpdates = append(filteredUpdates, s)
		}
	}

	// ... and missing services
	filteredDefined := map[flux.ServiceID]*ServiceUpdate{}
	for k, s := range definedMap {
		fr := s.filter(filters...)
		results[s.ServiceID] = fr
		if fr.Status != update.ReleaseStatusIgnored {
			filteredDefined[k] = s
		}
	}

	// Mark anything left over as skipped
	for id, _ := range filteredDefined {
		results[id] = update.ServiceResult{
			Status: update.ReleaseStatusSkipped,
			Error:  NotInCluster,
		}
	}
	return filteredUpdates, nil
}

func (s *ServiceUpdate) filter(filters ...ServiceFilter) update.ServiceResult {
	for _, f := range filters {
		fr := f.Filter(*s)
		if fr.Error != "" {
			return fr
		}
	}
	return update.ServiceResult{}
}

func (rc *ReleaseContext) FindDefinedServices() ([]*ServiceUpdate, error) {
	rc.Repo.RLock()
	defer rc.Repo.RUnlock()
	services, err := rc.Cluster.FindDefinedServices(rc.Repo.ManifestDir())
	if err != nil {
		return nil, err
	}

	var defined []*ServiceUpdate
	for id, paths := range services {
		switch len(paths) {
		case 1:
			def, err := ioutil.ReadFile(paths[0])
			if err != nil {
				return nil, err
			}
			defined = append(defined, &ServiceUpdate{
				ServiceID:     id,
				ManifestPath:  paths[0],
				ManifestBytes: def,
			})
		default:
			return nil, fmt.Errorf("multiple resource files found for service %s: %s", id, strings.Join(paths, ", "))
		}
	}
	return defined, nil
}

// Shortcut for this
func (rc *ReleaseContext) ServicesWithPolicy(p policy.Policy) (flux.ServiceIDSet, error) {
	rc.Repo.RLock()
	defer rc.Repo.RUnlock()
	return rc.Cluster.ServicesWithPolicy(rc.Repo.ManifestDir(), p)
}

type ServiceFilter interface {
	Filter(ServiceUpdate) update.ServiceResult
}

type SpecificImageFilter struct {
	Img flux.ImageID
}

func (f *SpecificImageFilter) Filter(u ServiceUpdate) update.ServiceResult {
	// If there are no containers, then we can't check the image.
	if len(u.Service.Containers.Containers) == 0 {
		return update.ServiceResult{
			Status: update.ReleaseStatusIgnored,
			Error:  NotInCluster,
		}
	}
	// For each container in update
	for _, c := range u.Service.Containers.Containers {
		cID, _ := flux.ParseImageID(c.Image)
		// If container image == image in update
		if cID.HostNamespaceImage() == f.Img.HostNamespaceImage() {
			// We want to update this
			return update.ServiceResult{}
		}
	}
	return update.ServiceResult{
		Status: update.ReleaseStatusIgnored,
		Error:  DifferentImage,
	}
}

type ExcludeFilter struct {
	IDs []flux.ServiceID
}

func (f *ExcludeFilter) Filter(u ServiceUpdate) update.ServiceResult {
	for _, id := range f.IDs {
		if u.ServiceID == id {
			return update.ServiceResult{
				Status: update.ReleaseStatusIgnored,
				Error:  Excluded,
			}
		}
	}
	return update.ServiceResult{}
}

type IncludeFilter struct {
	IDs []flux.ServiceID
}

func (f *IncludeFilter) Filter(u ServiceUpdate) update.ServiceResult {
	for _, id := range f.IDs {
		if u.ServiceID == id {
			return update.ServiceResult{}
		}
	}
	return update.ServiceResult{
		Status: update.ReleaseStatusIgnored,
		Error:  NotIncluded,
	}
}

type LockedFilter struct {
	IDs []flux.ServiceID
}

func (f *LockedFilter) Filter(u ServiceUpdate) update.ServiceResult {
	for _, id := range f.IDs {
		if u.ServiceID == id {
			return update.ServiceResult{
				Status: update.ReleaseStatusSkipped,
				Error:  Locked,
			}
		}
	}
	return update.ServiceResult{}
}
