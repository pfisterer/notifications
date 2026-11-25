// Package roleprovider resolves the group recipients of an event to people, through the role-provider-service's API and its generated Go client.
package roleprovider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/httpx"
	roleclient "github.com/pfisterer/role-provider-service/client"
)

// Resolver turns a userset ("group:<id>" or "group:<id>#<relation>") into the addresses of its members.
type Resolver interface {
	Members(ctx context.Context, userset string) ([]string, error)
}

// HTTP asks the role provider (GET /v1/groups/{group}/members?recursive=true&relation=…) with a resolve or read token.
type HTTP struct {
	client *roleclient.ClientWithResponses
}

// NewHTTP returns the resolver for the role provider at baseURL.
func NewHTTP(baseURL, token string, timeout time.Duration) (*HTTP, error) {
	editor := func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
	c, err := roleclient.NewClientWithResponses(baseURL, roleclient.WithHTTPClient(httpx.RetryingClient(timeout)), roleclient.WithRequestEditorFn(editor))
	if err != nil {
		return nil, err
	}
	return &HTTP{client: c}, nil
}

// Members lists the people of userset, recursively; the relation after "#" ("group:x#dozent") narrows it, without one every member counts.
func (h *HTTP) Members(ctx context.Context, userset string) ([]string, error) {
	group, relation := Split(userset)
	recursive := true
	params := &roleclient.ListGroupMembersParams{Recursive: &recursive, Relation: &relation}
	resp, err := h.client.ListGroupMembersWithResponse(ctx, group, params)
	if err != nil {
		return nil, fmt.Errorf("role provider: %w", err)
	}
	if resp.StatusCode() == http.StatusNotFound {
		// A group that no longer exists has no members; retrying would not change that.
		return nil, nil
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("role provider: unexpected status %d", resp.StatusCode())
	}
	var out []string
	for _, m := range *resp.JSON200 {
		if email, ok := strings.CutPrefix(m, "user:"); ok {
			out = append(out, strings.ToLower(email))
		}
	}
	return out, nil
}

// Split separates "group:<id>#<relation>" into the group token and the relation ("member" when none is given).
func Split(userset string) (group, relation string) {
	if g, rel, ok := strings.Cut(userset, "#"); ok && rel != "" {
		return g, rel
	}
	return strings.TrimSuffix(userset, "#"), "member"
}

// Static is a resolver from a fixed table, for tests and for running without a role provider.
type Static map[string][]string

// Members returns the table's entry for userset.
func (s Static) Members(_ context.Context, userset string) ([]string, error) {
	return s[userset], nil
}
