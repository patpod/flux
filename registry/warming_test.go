package registry

import (
	"context"
	"testing"

	"github.com/pkg/errors"
)

func TestName(t *testing.T) {
	err := errors.Wrap(context.DeadlineExceeded, "getting remote manifest")
	t.Log(err.Error())
	err = errors.Cause(err)
	if err == context.DeadlineExceeded {
		t.Log("OK")
	} else {
		t.Log("Not OK")
	}
}
