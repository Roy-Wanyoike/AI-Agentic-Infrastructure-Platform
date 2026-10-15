package compliance

// stubs_test.go — test doubles for the narrow reader interfaces. The tests
// NEVER construct real audit/deployment/evaluation services: every collabora-
// tor is a stub, so the tests pin the contract of the interfaces, not of the
// concrete backends.

import (
	"context"
	"errors"
	"time"

	"agentos/internal/agents"
	"agentos/internal/apikeys"
	"agentos/internal/approvals"
	"agentos/internal/audit"
	"agentos/internal/deployments"
	"agentos/internal/evaluations"
	"agentos/internal/mcp"
	"agentos/internal/policies"
)

type stubAudit struct {
	headHash string
	length   int
	headErr  error
	entries  []*audit.Entry
	listErr  error
}

func (s *stubAudit) ChainHead(ctx context.Context, orgID string) (string, int, error) {
	return s.headHash, s.length, s.headErr
}

func (s *stubAudit) ListCtx(ctx context.Context, orgID string) ([]*audit.Entry, error) {
	return s.entries, s.listErr
}

type stubAgents struct {
	agent  *agents.Agent
	getErr error
	list   []*agents.Agent
}

func (s *stubAgents) GetAgentCtx(ctx context.Context, orgID, id string) (*agents.Agent, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.agent, nil
}

func (s *stubAgents) ListAgentsCtx(ctx context.Context, orgID string) ([]*agents.Agent, error) {
	return s.list, nil
}

type stubVersions struct {
	rows []*agents.ConfigVersion
}

func (s *stubVersions) ListVersionsCtx(ctx context.Context, orgID, agentID string) ([]*agents.ConfigVersion, error) {
	return s.rows, nil
}

type stubDeployments struct {
	rows []*deployments.Deployment
}

func (s *stubDeployments) ListDeploymentsCtx(ctx context.Context, orgID, agentID string) ([]*deployments.Deployment, error) {
	return s.rows, nil
}

type stubEvals struct {
	datasets []*evaluations.Dataset
	samples  []evaluations.RunSample
}

func (s *stubEvals) ListDatasets(ctx context.Context, orgID string) ([]*evaluations.Dataset, error) {
	return s.datasets, nil
}

func (s *stubEvals) ListRunSamples(ctx context.Context, orgID, agentID string, limit int) ([]evaluations.RunSample, error) {
	return s.samples, nil
}

type stubPolicies struct {
	rows []*policies.Policy
	err  error
}

func (s *stubPolicies) ListPoliciesCtx(ctx context.Context, orgID string) ([]*policies.Policy, error) {
	return s.rows, s.err
}

type stubOrgKeys struct {
	n   int
	err error
}

func (s *stubOrgKeys) ListKeysCtx(ctx context.Context, orgID string) ([]*apikeys.APIKey, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make([]*apikeys.APIKey, s.n)
	return out, nil
}

type stubAgentKeys struct {
	byAgent map[string][]*apikeys.AgentKey
}

func (s *stubAgentKeys) ListAgentKeys(ctx context.Context, orgID, agentID string) ([]*apikeys.AgentKey, error) {
	return s.byAgent[agentID], nil
}

type stubApprovals struct {
	rows []*approvals.Approval
}

func (s *stubApprovals) List(ctx context.Context, orgID, status string) ([]*approvals.Approval, error) {
	return s.rows, nil
}

type stubMCP struct {
	n int
}

func (s *stubMCP) ListServers(ctx context.Context, orgID string) ([]*mcp.Server, error) {
	out := make([]*mcp.Server, s.n)
	return out, nil
}

// fixedClock pins the service clock for deterministic artifacts.
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at.UTC() }
}

var errBoom = errors.New("collector backend unavailable")
