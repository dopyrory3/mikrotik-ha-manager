package plan

import (
	"context"
	"fmt"
)

// Writer is the subset of routeros.Client that Execute needs. It is an
// interface so the executor can be tested without a router; *routeros.Client
// satisfies it.
type Writer interface {
	Post(ctx context.Context, path string, body, out any) error // RouterOS "add" (HTTP PUT)
	Patch(ctx context.Context, path string, body, out any) error
	Delete(ctx context.Context, path string) error
	Command(ctx context.Context, path string, body, out any) error // HTTP POST
}

// Execute performs a single Op against the target router's client.
//
// Plans are executed one Op at a time, in order, stopping at the first
// error: the pre-apply backup is the first Op for each router, so a failed
// backup stops everything after it, and a failed write never has later
// writes (which may depend on it, e.g. via place-before) piled on top. The
// caller then re-runs drift detection to show exactly what landed.
func Execute(ctx context.Context, w Writer, op Op) error {
	var err error
	switch op.Method {
	case MethodCreate:
		err = w.Post(ctx, op.Path, op.Body, nil)
	case MethodUpdate:
		err = w.Patch(ctx, op.Path, op.Body, nil)
	case MethodDelete:
		err = w.Delete(ctx, op.Path)
	case MethodCommand:
		err = w.Command(ctx, op.Path, op.Body, nil)
	default:
		return fmt.Errorf("unknown method %q", op.Method)
	}
	if err != nil {
		return fmt.Errorf("router %s: %w", op.Router, err)
	}
	return nil
}
