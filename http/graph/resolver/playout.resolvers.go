package resolver

// This file will be automatically regenerated based on the schema, any resolver implementations
// will be copied through when generating and any unknown code will be moved to the end.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/datarhei/core/v16/http/graph/models"
	"github.com/datarhei/core/v16/playout"
	"github.com/datarhei/core/v16/restream"
)

// PlayoutStatus is the resolver for the playoutStatus field.
func (r *queryResolver) PlayoutStatus(ctx context.Context, id string, domain string, input string) (*models.RawAVstream, error) {
	user, _ := ctx.Value("user").(string)

	if !r.IAM.Enforce(user, domain, "process:"+id, "read") {
		return nil, fmt.Errorf("forbidden")
	}

	tid := restream.TaskID{
		ID:     id,
		Domain: domain,
	}

	addr, err := r.Restream.GetPlayout(tid, input)
	if err != nil {
		return nil, fmt.Errorf("unknown process or input: %w", err)
	}

	path := "/v1/status"

	data, err := r.playoutRequest(http.MethodGet, addr, path, "", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get status: %w", err)
	}

	status := playout.Status{}

	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	s := &models.RawAVstream{}
	s.UnmarshalPlayout(status)

	return s, nil
}
