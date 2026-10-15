// GENERATED FILE — do not edit by hand.
// Source: api/openapi.yaml via scripts/gen_portal_docs.py
// Regenerate: python3 scripts/gen_portal_docs.py
window.AGENTOS_API = {
 "meta": {
  "title": "AgentOS API",
  "version": "0.1.0",
  "summary": "Multi-tenant AI-agent infrastructure platform API",
  "openapi": "3.1.0",
  "servers": [
   {
    "url": "/api/v1",
    "description": "Canonical versioned base path."
   },
   {
    "url": "/",
    "description": "Legacy root where all /v1/* routes are served identically  (e.g. /v1/agents). Deprecated alias kept for backward compatibility  (cmd/worker and existing clients still use it)."
   }
  ],
  "pathCount": 179,
  "operationCount": 236,
  "schemaCount": 496,
  "generated": "2026-10-15",
  "source": "api/openapi.yaml"
 },
 "tags": [
  {
   "name": "a2a",
   "description": ""
  },
  {
   "name": "agents",
   "description": "Agent registry (multi-tenant)."
  },
  {
   "name": "api-keys",
   "description": ""
  },
  {
   "name": "approvals",
   "description": ""
  },
  {
   "name": "Audit",
   "description": ""
  },
  {
   "name": "audit",
   "description": ""
  },
  {
   "name": "auth",
   "description": "Registration and token issuance."
  },
  {
   "name": "batch",
   "description": ""
  },
  {
   "name": "billing",
   "description": ""
  },
  {
   "name": "budgets",
   "description": ""
  },
  {
   "name": "compliance",
   "description": ""
  },
  {
   "name": "connectors",
   "description": ""
  },
  {
   "name": "deployments",
   "description": ""
  },
  {
   "name": "Evaluations",
   "description": ""
  },
  {
   "name": "events",
   "description": ""
  },
  {
   "name": "gateway",
   "description": ""
  },
  {
   "name": "Governance",
   "description": ""
  },
  {
   "name": "Knowledge",
   "description": ""
  },
  {
   "name": "marketplace",
   "description": ""
  },
  {
   "name": "mcp",
   "description": ""
  },
  {
   "name": "mcp-servers",
   "description": ""
  },
  {
   "name": "Memory",
   "description": ""
  },
  {
   "name": "messaging",
   "description": ""
  },
  {
   "name": "notifications",
   "description": ""
  },
  {
   "name": "orchestration",
   "description": ""
  },
  {
   "name": "organization",
   "description": ""
  },
  {
   "name": "platform",
   "description": "Health, readiness and metrics."
  },
  {
   "name": "quality-gates",
   "description": ""
  },
  {
   "name": "queue",
   "description": "Worker task pull (dev pull model)."
  },
  {
   "name": "rate-limits",
   "description": ""
  },
  {
   "name": "residency",
   "description": ""
  },
  {
   "name": "run-control",
   "description": ""
  },
  {
   "name": "runs",
   "description": "Agent executions and their event streams."
  },
  {
   "name": "Schedules",
   "description": ""
  },
  {
   "name": "scim",
   "description": ""
  },
  {
   "name": "secrets",
   "description": ""
  },
  {
   "name": "showback",
   "description": ""
  },
  {
   "name": "synth",
   "description": ""
  },
  {
   "name": "Tools",
   "description": ""
  },
  {
   "name": "Usage",
   "description": ""
  },
  {
   "name": "usage",
   "description": ""
  },
  {
   "name": "webhooks",
   "description": ""
  },
  {
   "name": "workflow-runs",
   "description": ""
  },
  {
   "name": "workflows",
   "description": ""
  }
 ],
 "groups": [
  {
   "tag": "a2a",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/a2a/agents",
     "operationId": "createA2AAgentCard",
     "summary": "Publish an org agent as an A2A Agent Card",
     "description": "",
     "permission": "agents.write (OWNER/ADMIN)",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "A2ACardInput",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Card published (registry row).",
       "schema": "A2ACardEnvelope"
      },
      {
       "status": "400",
       "description": "Invalid request body",
       "schema": "A2AError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "A2AError"
      },
      {
       "status": "403",
       "description": "Authenticated principal lacks the required permission",
       "schema": "A2AError"
      },
      {
       "status": "422",
       "description": "Validation error (missing name/version, unknown or cross-org agent, bad endpoint_url).",
       "schema": "A2AError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/a2a/agents",
     "operationId": "listA2AAgentCards",
     "summary": "List the caller org's Agent Cards",
     "description": "",
     "permission": "connectors.read (MEMBER+)",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Registry rows of exactly one organization (possibly empty).",
       "schema": "A2ACardListEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "A2AError"
      },
      {
       "status": "403",
       "description": "Authenticated principal lacks the required permission",
       "schema": "A2AError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/a2a/agents/{id}",
     "operationId": "getA2AAgentCard",
     "summary": "Get one Agent Card (registry row)",
     "description": "Unknown and foreign-tenant ids are the same 404 (no existence leak).",
     "permission": "connectors.read (MEMBER+)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent Card id (UUID, as returned at publication)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The registry row.",
       "schema": "A2ACardEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "A2AError"
      },
      {
       "status": "404",
       "description": "Unknown or foreign id",
       "schema": "A2AError"
      }
     ]
    },
    {
     "method": "PATCH",
     "path": "/a2a/agents/{id}",
     "operationId": "patchA2AAgentCard",
     "summary": "Update an Agent Card (partial semantics)",
     "description": "",
     "permission": "agents.write (OWNER/ADMIN)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent Card id (UUID, as returned at publication)."
      }
     ],
     "bodySchema": "A2ACardPatch",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Updated registry row.",
       "schema": "A2ACardEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "A2AError"
      },
      {
       "status": "403",
       "description": "Authenticated principal lacks the required permission",
       "schema": "A2AError"
      },
      {
       "status": "404",
       "description": "Unknown or foreign id",
       "schema": "A2AError"
      },
      {
       "status": "422",
       "description": "The patched state fails validation (e.g. empty name, unknown agent re-link).",
       "schema": "A2AError"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/a2a/agents/{id}",
     "operationId": "deleteA2AAgentCard",
     "summary": "Delete an Agent Card",
     "description": "Hard-deletes the card; the discovery surface stops answering. Audited (a2a.card_deleted).",
     "permission": "agents.write (OWNER/ADMIN)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent Card id (UUID, as returned at publication)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deleted",
       "schema": "A2ADeletedEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "A2AError"
      },
      {
       "status": "403",
       "description": "Authenticated principal lacks the required permission",
       "schema": "A2AError"
      },
      {
       "status": "404",
       "description": "Unknown or foreign id",
       "schema": "A2AError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/a2a/agents/{id}/card",
     "operationId": "getA2ACanonicalAgentCard",
     "summary": "Discover the canonical A2A Agent Card",
     "description": "",
     "permission": "agents.read (all roles)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent Card id (UUID, as returned at publication)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The canonical Agent Card.",
       "schema": "A2ACanonicalCard"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "A2AError"
      },
      {
       "status": "404",
       "description": "Unknown or foreign id (no existence leak)",
       "schema": "A2AError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/a2a/agents/{id}/message",
     "operationId": "sendA2AAgentMessage",
     "summary": "Receive an inbound A2A message for one card",
     "description": "",
     "permission": "runs.execute (OWNER/ADMIN/MEMBER)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent Card id (UUID, as returned at publication)."
      }
     ],
     "bodySchema": "A2AMessageRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "202",
       "description": "Message accepted and routed to the card's agent.",
       "schema": "A2AMessageResult"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "A2AError"
      },
      {
       "status": "403",
       "description": "Authenticated principal lacks the required permission",
       "schema": "A2AError"
      },
      {
       "status": "404",
       "description": "Unknown or foreign card (no existence leak)",
       "schema": "A2AError"
      },
      {
       "status": "422",
       "description": "Empty message",
       "schema": "A2AError"
      },
      {
       "status": "503",
       "description": "No run sink wired (degraded deployment)",
       "schema": "A2AError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/a2a/delegate",
     "operationId": "delegateA2AMessage",
     "summary": "Delegate a message to a remote A2A agent (governed)",
     "description": "",
     "permission": "runs.execute (OWNER/ADMIN/MEMBER)",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "A2ADelegateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Delegation executed (the remote answered; outcome is the payload).",
       "schema": "A2ADelegateResult"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "A2AError"
      },
      {
       "status": "403",
       "description": "Policy denial (no wire call was made).",
       "schema": "A2ADelegationDenied"
      },
      {
       "status": "404",
       "description": "Unknown or foreign card_id (no existence leak)",
       "schema": "A2AError"
      },
      {
       "status": "422",
       "description": "Validation error (missing message/target, unfetchable or invalid card_url).",
       "schema": "A2AError"
      },
      {
       "status": "502",
       "description": "The target endpoint was unreachable (transport error, timeout).",
       "schema": "A2AError"
      }
     ]
    }
   ]
  },
  {
   "tag": "agents",
   "description": "Agent registry (multi-tenant).",
   "operations": [
    {
     "method": "GET",
     "path": "/agents",
     "operationId": "listAgents",
     "summary": "List agents in the caller's organization",
     "description": "Returns a JSON **array** (not an envelope) of agents belonging to the requested organization. `organization_id` query parameter defaults to the authenticated principal's organization; cross-organization access is forbidden. Unpaginated today.",
     "permission": "agents.read",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "organization_id",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Organization to list; defaults to the caller's organization."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Array of agents (possibly empty).",
       "schema": "Agent[]"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/agents/create",
     "operationId": "createAgent",
     "summary": "Create an agent",
     "description": "Creates an agent in `DRAFT` status and an initial AgentVersion (version 1) which becomes `CurrentVersionID`. Required fields: `name`, `instructions`, `model` (service-side validation); `organization_id` defaults to the caller's organization.",
     "permission": "agents.write",
     "auth": "bearerToken or apiKey",
     "params": [],
     "bodySchema": "CreateAgentRequest",
     "bodyRequired": true,
     "bodyExample": "{\n  \"organization_id\": \"org-1\",\n  \"name\": \"Support Agent\",\n  \"description\": \"Demo customer support agent\",\n  \"instructions\": \"Answer simple questions\",\n  \"model\": \"gpt-4o-mini\"\n}",
     "responses": [
      {
       "status": "201",
       "description": "Agent created.",
       "schema": "Agent"
      },
      {
       "status": "400",
       "description": "Invalid body or validation failure (name/instructions/model required).",
       "schema": "Error"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/agents/{id}",
     "operationId": "getAgent",
     "summary": "Get an agent by ID",
     "description": "",
     "permission": "agents.read",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent identifier (e.g. `agent-1`)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The agent.",
       "schema": "Agent"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/agents/{id}",
     "operationId": "updateAgent",
     "summary": "Update an agent's draft fields (merge; immutable fields rejected explicitly)",
     "description": "Write operation (agents.write, exactly OWNER/ADMIN). Merges the provided DRAFT fields onto the live agent row: name, description, instructions (the system prompt) and model. The version contract's canonical `system_prompt` key is accepted as an alias of `instructions`; supplying both with conflicting values is a 400. Only provided fields change (merge semantics); provided name/instructions/model must be non-blank strings. IMMUTABILITY: any other request key \u2014 e.g. tools, config, temperature, params, status, version, id, organization_id, created_at, updated_at \u2014 is governed by the immutable published version snapshots (or identity bookkeeping) and is REJECTED with 400 IMMUTABLE_FIELDS listing the offending keys. Fields are never silently dropped; publish/rollback mutate those values through the version lifecycle, not through this endpoint. The tenant comes from the auth claims; cross-organization agents surface as 404 (no existence leak). Success writes an `agent.updated` audit entry (metadata carries the changed field names) and returns the updated agent in the same representation as GET /agents/{id} (the legacy PascalCase Agent encoding).",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent id (UUID, as returned by agent creation)."
      }
     ],
     "bodySchema": "AglUpdateAgentRequest",
     "bodyRequired": true,
     "bodyExample": "{\n  \"name\": \"Support Agent v2\",\n  \"description\": \"Handles billing questions\",\n  \"system_prompt\": \"You are a precise support agent. Cite docs.\",\n  \"model\": \"gpt-4o-mini\"\n}",
     "responses": [
      {
       "status": "200",
       "description": "Agent updated; the response is the updated agent (same shape as GET /agents/{id}).",
       "schema": "AglAgent"
      },
      {
       "status": "400",
       "description": "Malformed JSON body (INVALID_REQUEST), conflicting  instructions/system_prompt values (INVALID_REQUEST), or the body  contains immutable/unsupported fields outside the draft update  surface (IMMUTABLE_FIELDS \u2014 the message enumerates every  rejected key).",
       "schema": "AglErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "AglErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission (agents.write is OWNER/ADMIN only).",
       "schema": "AglErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown agent or agent of another organization (AGENT_NOT_FOUND).",
       "schema": "AglErrorEnvelope"
      },
      {
       "status": "422",
       "description": "Validation error (VALIDATION_ERROR): blank name/instructions/model  when provided, non-string values, or no updatable field present.",
       "schema": "AglErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (details are never surfaced).",
       "schema": "AglErrorEnvelope"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/agents/{id}",
     "operationId": "deleteAgent",
     "summary": "Delete an agent (hard delete, guarded by run references)",
     "description": "Write operation (agents.write, exactly OWNER/ADMIN). Performs a HARD delete of the agent row within the caller's tenant \u2014 there is no soft-delete: a successful delete removes the agents row and subsequent GETs 404. Agent config versions, deployments (including canary rows), schedules and memory snippets are removed by their ON DELETE CASCADE foreign keys. REFERENCE GUARD: runs.agent_id (migration 001, fk_runs_agent) has no ON DELETE action, so the durable store itself refuses the delete while runs still reference the agent. The endpoint checks first and answers 409 AGENT_HAS_RUNS (message carries the run count) instead of surfacing a raw foreign-key 500; the agent remains untouched and no audit entry is written for a guarded delete. Delete the runs (or reassign them) before deleting the agent. The tenant comes from the auth claims; cross-organization or unknown agents surface as 404 (no existence leak, zero writes). Success writes an `agent.deleted` audit entry.",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent id (UUID, as returned by agent creation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Agent deleted.",
       "schema": "AglDeleteAgentResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "AglErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission (agents.write is OWNER/ADMIN only).",
       "schema": "AglErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown agent or agent of another organization (AGENT_NOT_FOUND).",
       "schema": "AglErrorEnvelope"
      },
      {
       "status": "409",
       "description": "The agent still has runs referencing it (AGENT_HAS_RUNS); the  delete was refused and the agent is unchanged. This mirrors the  durable runs FK (no ON DELETE action).",
       "schema": "AglErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (details are never surfaced).",
       "schema": "AglErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/agents/{id}/rollback",
     "operationId": "rollbackAgent",
     "summary": "Re-point the agent to a target version and restore its snapshot config",
     "description": "",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent identifier (e.g. `agent-1`)."
      }
     ],
     "bodySchema": "RollbackAgentRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Target version is now the agent's current version (live config restored).",
       "schema": "CurrentVersionResponse"
      },
      {
       "status": "400",
       "description": "Malformed request body.",
       "schema": "ErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      },
      {
       "status": "422",
       "description": "target_version missing or not a positive integer.",
       "schema": "ErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/agents/{id}/versions",
     "operationId": "listAgentVersions",
     "summary": "List an agent's immutable config versions (ascending)",
     "description": "",
     "permission": "agents.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent identifier (e.g. `agent-1`)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Config versions of the agent, ascending by version number.",
       "schema": "AgentVersionsResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/agents/{id}/versions/create",
     "operationId": "createAgentVersion",
     "summary": "Snapshot the agent's current configuration into a new draft version",
     "description": "",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent identifier (e.g. `agent-1`)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Draft version created (numbered after the highest existing version).",
       "schema": "VersionNumberResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/agents/{id}/versions/{version}/publish",
     "operationId": "publishAgentVersion",
     "summary": "Mark a draft version immutable (published) and re-point the agent to it",
     "description": "",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent identifier (e.g. `agent-1`)."
      },
      {
       "name": "version",
       "in": "path",
       "required": true,
       "type": "integer",
       "description": "Version number to publish."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Version published (idempotent re-publish returns the same version).",
       "schema": "VersionNumberResponse"
      },
      {
       "status": "400",
       "description": "Version path segment is not a positive integer.",
       "schema": "ErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      },
      {
       "status": "409",
       "description": "Version is archived (revive it via rollback instead).",
       "schema": "ErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/agents/{id}/versions/diff",
     "operationId": "diffAgentVersions",
     "summary": "Structured field-level diff between two config versions of one agent",
     "description": "Compares the immutable snapshot documents of version `from` and version `to` of the same agent. The response always contains one entry per comparable field (model, system_prompt, temperature, params, tools, description \u2014 null when a snapshot does not capture the field) plus any additional snapshot keys, sorted, so future snapshot fields diff too. `changed` is true when the from/to values differ. Both versions are resolved strictly within the caller's organization; unknown versions, cross-agent version numbers and foreign-tenant agents surface as 404.",
     "permission": "agents.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent identifier (UUID v4 string)."
      },
      {
       "name": "from",
       "in": "query",
       "required": true,
       "type": "integer",
       "description": "Base version number of the diff (positive integer; may equal `to`)."
      },
      {
       "name": "to",
       "in": "query",
       "required": true,
       "type": "integer",
       "description": "Target version number of the diff (positive integer; may equal `from`)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The field-level diff document.",
       "schema": "VdfVersionDiffResponse"
      },
      {
       "status": "400",
       "description": "from/to missing, non-integer, or not positive.",
       "schema": "VdfErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "VdfErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the agents.read permission.",
       "schema": "VdfErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Agent or version not found within the caller's organization  (error.code AGENT_NOT_FOUND or VERSION_NOT_FOUND).",
       "schema": "VdfErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "VdfErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/agents/{id}/keys",
     "operationId": "createAgentKey",
     "summary": "Mint an agent-scoped key (plaintext exactly once)",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": "AgentKeyCreate",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Minted. `value` is the ONLY delivery of the plaintext.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "AgentKeyError"
      },
      {
       "status": "403",
       "description": "Not permitted (agents.write is OWNER/ADMIN).",
       "schema": "AgentKeyError"
      },
      {
       "status": "404",
       "description": "Unknown agent/key or cross-tenant probe (no existence leak).",
       "schema": "AgentKeyError"
      },
      {
       "status": "422",
       "description": "Validation error (blank name, bad expiry, empty allow-list entries).",
       "schema": "AgentKeyError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/agents/{id}/keys",
     "operationId": "listAgentKeys",
     "summary": "List keys of one agent (no secrets)",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Key list.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "AgentKeyError"
      },
      {
       "status": "403",
       "description": "Not permitted (agents.write is OWNER/ADMIN).",
       "schema": "AgentKeyError"
      },
      {
       "status": "404",
       "description": "Unknown agent/key or cross-tenant probe (no existence leak).",
       "schema": "AgentKeyError"
      }
     ]
    },
    {
     "method": "PATCH",
     "path": "/agents/{id}/keys/{keyID}",
     "operationId": "updateAgentKey",
     "summary": "Update name / allow-list / expiry",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      },
      {
       "name": "keyID",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": "AgentKeyUpdate",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Updated.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "AgentKeyError"
      },
      {
       "status": "403",
       "description": "Not permitted (agents.write is OWNER/ADMIN).",
       "schema": "AgentKeyError"
      },
      {
       "status": "404",
       "description": "Unknown agent/key or cross-tenant probe (no existence leak).",
       "schema": "AgentKeyError"
      },
      {
       "status": "422",
       "description": "Validation error (blank name, bad expiry, empty allow-list entries).",
       "schema": "AgentKeyError"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/agents/{id}/keys/{keyID}",
     "operationId": "revokeAgentKey",
     "summary": "Revoke an agent key",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      },
      {
       "name": "keyID",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Revoked.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "AgentKeyError"
      },
      {
       "status": "403",
       "description": "Not permitted (agents.write is OWNER/ADMIN).",
       "schema": "AgentKeyError"
      },
      {
       "status": "404",
       "description": "Unknown agent/key or cross-tenant probe (no existence leak).",
       "schema": "AgentKeyError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/agents/{id}/export",
     "operationId": "exportAgent",
     "summary": "Export one agent as a portable configuration-only bundle (issue",
     "description": "",
     "permission": "agents.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent id (UUID, as returned by create/import/clone/list)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The portable bundle (configuration only).",
       "schema": "AgentExportBundle"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "PortErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission (agents.write is OWNER/ADMIN only; agents.read is granted to every role).",
       "schema": "PortErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown agent or agent of another organization (AGENT_NOT_FOUND).",
       "schema": "PortErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (details are never surfaced).",
       "schema": "PortErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/agents/import",
     "operationId": "importAgent",
     "summary": "Import a bundle as a NEW agent with fresh identity (issue",
     "description": "",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "AgentExportBundle",
     "bodyRequired": true,
     "bodyExample": "{\n  \"schema_version\": \"1\",\n  \"exported_at\": \"2026-01-01T00:00:00Z\",\n  \"agent\": {\n    \"name\": \"Support Agent\",\n    \"description\": \"front-line helper\",\n    \"instructions\": \"be helpful\",\n    \"model\": \"gpt-4o-mini\",\n    \"tools\": []\n  }\n}",
     "responses": [
      {
       "status": "201",
       "description": "Agent imported with fresh identity in the caller's organization.",
       "schema": "PortAgent"
      },
      {
       "status": "400",
       "description": "Malformed or missing JSON body (INVALID_REQUEST).",
       "schema": "PortErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "PortErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission (agents.write is OWNER/ADMIN only; agents.read is granted to every role).",
       "schema": "PortErrorEnvelope"
      },
      {
       "status": "422",
       "description": "",
       "schema": "PortErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (details are never surfaced).",
       "schema": "PortErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/agents/{id}/clone",
     "operationId": "cloneAgent",
     "summary": "Deep-copy one agent within the SAME organization (issue",
     "description": "",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent id (UUID, as returned by create/import/clone/list)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Clone created (DRAFT, \"-copy\" name suffix) in the caller's organization.",
       "schema": "PortAgent"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "PortErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission (agents.write is OWNER/ADMIN only; agents.read is granted to every role).",
       "schema": "PortErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown source agent or agent of another organization (AGENT_NOT_FOUND).",
       "schema": "PortErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (details are never surfaced).",
       "schema": "PortErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "api-keys",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/api-keys",
     "operationId": "createApiKey",
     "summary": "Mint a new API key for the caller's organization (value returned exactly once)",
     "description": "Write operation (agents.write, OWNER/ADMIN). Body carries the key `name`; the server generates the secret (`ak_` + 32 random bytes), persists only its SHA-256 hash, and returns the plaintext `value` EXACTLY ONCE as the top-level sibling of the metadata object \u2014 this is the only response in the whole API that carries the key. The plaintext cannot be retrieved again afterwards. The key authenticates immediately (X-API-Key header) as an OWNER identity of the caller's organization. The tenant is derived from the auth claims \u2014 any client-supplied organization id is ignored. Minting is audit-logged (`api_key.created`, no key material in metadata).",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "ApiKeyCreateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Key minted. The metadata object never contains the value or the  hash; `value` (the plaintext credential) is returned exactly once  and must be stored by the client now.",
       "schema": "ApiKeyCreatedResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body (error.code INVALID_REQUEST).",
       "schema": "ApiKeyErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ApiKeyErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "ApiKeyErrorEnvelope"
      },
      {
       "status": "422",
       "description": "Validation error: missing or blank key name (error.code  VALIDATION_ERROR).",
       "schema": "ApiKeyErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (details are never surfaced).",
       "schema": "ApiKeyErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/api-keys",
     "operationId": "listApiKeys",
     "summary": "List API key metadata for the caller's organization (never values or hashes)",
     "description": "Read operation (runs.execute, MEMBER+ \u2014 viewers are excluded because credential inventories are sensitive). Returns metadata only: id, name, the display `prefix` (`ak_` + the first 8 secret characters, kept for identification; the server only ever matches the SHA-256 hash of the FULL value, so the prefix authenticates nothing), created_by, created_at and the revoked flag. The response schema has no value field and no full hash, and the server never includes them. Results are ordered newest first. Revoked keys remain listed with \"revoked\": true. Note: the api_keys table also tracks last_used_at and the revocation timestamp, but the service model does not project them yet \u2014 they are therefore absent from this contract until the service exposes them (deliberate omission, not an accident).",
     "permission": "runs.execute",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Metadata list (possibly empty; values and hashes are structurally absent).",
       "schema": "ApiKeyListResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ApiKeyErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "ApiKeyErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ApiKeyErrorEnvelope"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/api-keys/{id}",
     "operationId": "revokeApiKey",
     "summary": "Revoke one API key (immediate; idempotent on already-revoked keys)",
     "description": "Write operation (agents.write, OWNER/ADMIN). Revocation is immediate: the key stops authenticating on the very next request (it is resolved by hash and rejected once revoked). Re-revoking an already-revoked key is idempotent and stays 200; unknown or foreign-organization ids surface as 404 with no existence leak across tenants. Revocation is audit-logged (`api_key.revoked`).",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "API key id (UUID, as returned by the mint operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Key revoked (or already revoked \u2014 idempotent).",
       "schema": "ApiKeyRevokeResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ApiKeyErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "ApiKeyErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Key not found within the caller's organization (error.code API_KEY_NOT_FOUND).",
       "schema": "ApiKeyErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ApiKeyErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "approvals",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/approvals",
     "operationId": "listApprovals",
     "summary": "List the tenant's approvals",
     "description": "",
     "permission": "approvals.read",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "status",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Filter by status (pending|approved|rejected|cancelled)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Approvals (unpaginated today).",
       "schema": "ApprovalListResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server error.",
       "schema": "WfErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/approvals/{id}",
     "operationId": "getApproval",
     "summary": "Get one approval",
     "description": "",
     "permission": "approvals.read",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Approval identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The approval.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Resource not found (or owned by another tenant \u2014 cross-tenant access surfaces as 404, never 403).",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server error.",
       "schema": "WfErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/approvals/{id}/decide",
     "operationId": "decideApproval",
     "summary": "Approve or reject an approval",
     "description": "Sets the approver from the authenticated principal and decided_at to now. When the decision is `approved` and a run is linked, the paused run is resumed (status back to `pending`). Deciding twice returns 409.",
     "permission": "approvals.decide",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Approval identifier (UUID v4 string)."
      }
     ],
     "bodySchema": "DecideApprovalRequest",
     "bodyRequired": true,
     "bodyExample": "{\n  \"decision\": \"approved\",\n  \"reason\": \"Verified the deployment plan\"\n}",
     "responses": [
      {
       "status": "200",
       "description": "Decision recorded.",
       "schema": "object"
      },
      {
       "status": "400",
       "description": "Malformed JSON body.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Resource not found (or owned by another tenant \u2014 cross-tenant access surfaces as 404, never 403).",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "409",
       "description": "The approval has already been decided.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "422",
       "description": "decision must be `approved` or `rejected`.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server error.",
       "schema": "WfErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/approvals/{id}/delegate",
     "operationId": "delegateApproval",
     "summary": "Delegate a pending approval to an organization member",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": "ApprovalDelegationInput",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Delegation recorded (approval + chain entry).",
       "schema": "object"
      },
      {
       "status": "400",
       "description": "Malformed request (bad JSON body or RFC3339 bound).",
       "schema": "ApprovalsEnterpriseError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ApprovalsEnterpriseError"
      },
      {
       "status": "403",
       "description": "Not permitted for this role (evidence needs OWNER/ADMIN).",
       "schema": "ApprovalsEnterpriseError"
      },
      {
       "status": "404",
       "description": "Unknown id or cross-tenant probe (no existence leak).",
       "schema": "ApprovalsEnterpriseError"
      },
      {
       "status": "409",
       "description": "The approval is not pending.",
       "schema": "ApprovalsEnterpriseError"
      },
      {
       "status": "422",
       "description": "Validation error (empty delegatee, non-member delegatee, bad policy, inverted window).",
       "schema": "ApprovalsEnterpriseError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/approvals/escalate-due",
     "operationId": "escalateDueApprovals",
     "summary": "Sweep pending approvals past their escalation SLA",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The approvals escalated by this call.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ApprovalsEnterpriseError"
      },
      {
       "status": "403",
       "description": "Not permitted for this role (evidence needs OWNER/ADMIN).",
       "schema": "ApprovalsEnterpriseError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/approvals/escalation-policy",
     "operationId": "getApprovalEscalationPolicy",
     "summary": "Get the org escalation SLA policy (disabled default when unset)",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The policy.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ApprovalsEnterpriseError"
      },
      {
       "status": "403",
       "description": "Not permitted for this role (evidence needs OWNER/ADMIN).",
       "schema": "ApprovalsEnterpriseError"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/approvals/escalation-policy",
     "operationId": "setApprovalEscalationPolicy",
     "summary": "Write the org escalation SLA policy",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [],
     "bodySchema": "ApprovalEscalationPolicyInput",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The stored policy.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ApprovalsEnterpriseError"
      },
      {
       "status": "403",
       "description": "Not permitted for this role (evidence needs OWNER/ADMIN).",
       "schema": "ApprovalsEnterpriseError"
      },
      {
       "status": "422",
       "description": "Validation error (empty delegatee, non-member delegatee, bad policy, inverted window).",
       "schema": "ApprovalsEnterpriseError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/approvals/evidence",
     "operationId": "exportApprovalEvidence",
     "summary": "Byte-deterministic NDJSON oversight snapshot for a window",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "from",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Window start (inclusive, RFC3339; open when omitted)."
      },
      {
       "name": "to",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Window end (inclusive, RFC3339; open when omitted)."
      },
      {
       "name": "format",
       "in": "query",
       "required": false,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "NDJSON snapshot (X-Evidence-Count carries the event count).",
       "schema": null
      },
      {
       "status": "400",
       "description": "Malformed request (bad JSON body or RFC3339 bound).",
       "schema": "ApprovalsEnterpriseError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ApprovalsEnterpriseError"
      },
      {
       "status": "403",
       "description": "Not permitted for this role (evidence needs OWNER/ADMIN).",
       "schema": "ApprovalsEnterpriseError"
      }
     ]
    }
   ]
  },
  {
   "tag": "Audit",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/audit-events",
     "operationId": "listAuditEvents",
     "summary": "List the caller organization's audit trail (newest first, keyset-paginated)",
     "description": "The append-only audit trail of the caller's organization, newest first. The tenant is taken from the auth claims only \u2014 client-supplied organization ids are never trusted. Pagination is keyset-based on (created_at, id): pass the returned next_cursor back as the cursor query parameter; appends between pages can neither duplicate nor skip rows. next_cursor is the empty string when the trail is exhausted.",
     "permission": "audit.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "limit",
       "in": "query",
       "required": false,
       "type": "integer",
       "description": "Page size (1-200; defaults to 50). Values above 200 are clamped  server-side instead of erroring.  "
      },
      {
       "name": "cursor",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Opaque continuation token from a previous page's next_cursor."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "One page of the tenant's audit trail (empty array when nothing is recorded yet).",
       "schema": "AuditEventsResponse"
      },
      {
       "status": "400",
       "description": "Invalid query (non-integer/zero limit, or malformed cursor).",
       "schema": "AuditErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (e.g. store unavailable).",
       "schema": "AuditErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "audit",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/audit/verify",
     "operationId": "verifyAuditChain",
     "summary": "Verify the tamper-evident audit chain of the caller's organization",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "organization_id",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Must agree with the authenticated claims (404 otherwise)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Chain verdict.",
       "schema": "AudChainReport"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "AudError"
      },
      {
       "status": "403",
       "description": "Authenticated but not permitted (audit.read is OWNER/ADMIN).",
       "schema": "AudError"
      },
      {
       "status": "404",
       "description": "Cross-tenant probe \u2014 no existence leak.",
       "schema": "AudError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/audit/export",
     "operationId": "exportAuditTrail",
     "summary": "Export the org's audit trail as deterministic JSONL evidence",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "format",
       "in": "query",
       "required": true,
       "type": "string",
       "description": ""
      },
      {
       "name": "organization_id",
       "in": "query",
       "required": false,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "NDJSON evidence snapshot.",
       "schema": null
      },
      {
       "status": "400",
       "description": "Invalid request (e.g. format must be jsonl).",
       "schema": "AudError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "AudError"
      },
      {
       "status": "403",
       "description": "Authenticated but not permitted (audit.read is OWNER/ADMIN).",
       "schema": "AudError"
      },
      {
       "status": "404",
       "description": "Cross-tenant probe \u2014 no existence leak.",
       "schema": "AudError"
      }
     ]
    }
   ]
  },
  {
   "tag": "auth",
   "description": "Registration and token issuance.",
   "operations": [
    {
     "method": "POST",
     "path": "/auth/login",
     "operationId": "login",
     "summary": "Exchange credentials for a bearer token",
     "description": "Returns an HMAC-SHA256 JWT-style token (24h expiry) carrying claims `user_id`, `organization_id`, `email`, `role`, `exp`. Use it as `Authorization: Bearer <token>` afterwards. No authentication required.",
     "permission": "",
     "auth": "public",
     "params": [],
     "bodySchema": "LoginRequest",
     "bodyRequired": true,
     "bodyExample": "{\n  \"email\": \"owner@acme.dev\",\n  \"password\": \"s3cret-password\"\n}",
     "responses": [
      {
       "status": "200",
       "description": "Token issued.",
       "schema": "LoginResponse"
      },
      {
       "status": "401",
       "description": "Invalid credentials (plain text body today).",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/auth/register",
     "operationId": "register",
     "summary": "Register an organization and its first OWNER user",
     "description": "Creates a new organization (ID pattern `org-<n>`) and a user with the default role `OWNER`. No authentication required. The response intentionally includes the stored user record as implemented today \u2014 including `PasswordHash` (bcrypt digest). This is a known contract wart tracked for removal; clients must treat it as opaque.",
     "permission": "",
     "auth": "public",
     "params": [],
     "bodySchema": "RegisterRequest",
     "bodyRequired": true,
     "bodyExample": "{\n  \"organization\": \"Acme AI\",\n  \"email\": \"owner@acme.dev\",\n  \"password\": \"s3cret-password\"\n}",
     "responses": [
      {
       "status": "201",
       "description": "Organization and user created.",
       "schema": "RegisterResponse"
      },
      {
       "status": "400",
       "description": "Missing field, invalid JSON body, or email already registered.  Body is plain text today (http.Error); structured error is the  target model.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/auth/logout",
     "operationId": "logout",
     "summary": "Revoke the presented session token server-side (idempotent)",
     "description": "Invalidates the presented HMAC session token immediately and server-side. The token's revocation reference lands on the deny list with a TTL equal to the token's remaining lifetime, so the entry self-expires when the token would have expired anyway; every subsequent request with the same token fails (401, \"token has been revoked\") on any protected endpoint. Idempotent: logging out an already-revoked or already-expired token answers 204 again. The credential is the presented bearer token only \u2014 no body, and API keys are not accepted (machine credentials are revoked through DELETE /api-keys/{id}). A successful logout is audit-logged (auth.logout) with the token reference, never token material. Tokens minted before issue #76 (no jti claim) are revoked under the SHA-256 of their full token string and are equally killable.",
     "permission": "",
     "auth": "bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "204",
       "description": "Token revoked (or already revoked/expired \u2014 idempotent). No  response body. The token is dead on every protected endpoint  from this moment.",
       "schema": null
      },
      {
       "status": "401",
       "description": "Missing or malformed Authorization header (error.code  UNAUTHORIZED), or the presented credential is not a validly  signed session token at all (error.code TOKEN_INVALID). Logout  of a merely expired or already-revoked token is NOT this case \u2014  it answers 204.",
       "schema": "AuthLifecycleErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (details are never surfaced).",
       "schema": "AuthLifecycleErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/auth/refresh",
     "operationId": "refresh",
     "summary": "Exchange a valid session token for a fresh one (single-use rotation)",
     "description": "Rotates the presented session token: the old token is revoked server-side and a fresh token (rotated jti, full new 24h window, sliding session) is returned. Single-use by construction \u2014 the old token is revoked BEFORE the new one is minted, so replaying it (for another refresh or any protected endpoint) answers 401 TOKEN_REVOKED; a rotation can never leave two live tokens. The fresh claims are re-derived from the user's CURRENT stored identity row, so SCIM deprovisioning (active=false) and role changes apply exactly as they do at password login: a deprovisioned user is refused with 401 ACCOUNT_DISABLED (and no rotation happens \u2014 the old token's validity is untouched by a failed refresh), and an identity that can no longer be resolved is refused with 401 TOKEN_INVALID. A deny-list backend outage also fails closed (401). Successful rotations are audit-logged (auth.refresh) with the old and new token references, never token material.",
     "permission": "",
     "auth": "bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Rotation complete. The old token is already dead server-side;  the response carries the only live credential.",
       "schema": "AuthLifecycleRefreshResponse"
      },
      {
       "status": "401",
       "description": "The presented token is invalid, malformed, expired, revoked  (error.code TOKEN_INVALID or TOKEN_REVOKED \u2014 a revoked token is  reported distinctly so single-use replay is observable), or the  account has been deprovisioned (error.code ACCOUNT_DISABLED).  UNAUTHORIZED is used when the Authorization header is missing  or not a bearer token.",
       "schema": "AuthLifecycleErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (details are never surfaced).",
       "schema": "AuthLifecycleErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/auth/sso/{org_slug}/login",
     "operationId": "beginSsoLogin",
     "summary": "Start the OIDC login flow (302 to the IdP authorize URL)",
     "description": "Resolves the organization by its URL slug, validates the stored sso_config against the live discovery document and returns a 302 redirect to the IdP authorization endpoint carrying client_id, redirect_uri (the derived callback `<scheme>://<host>/auth/sso/callback`, overridable by a config-pinned redirect_uri), state, nonce and scopes. Unknown slugs and unconfigured organizations both answer 404. No authentication header applies: this endpoint exists to start a browser login.",
     "permission": "",
     "auth": "public",
     "params": [
      {
       "name": "org_slug",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "URL slug of the organization (case-insensitive)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "302",
       "description": "Redirect to the IdP authorization endpoint (Location header).",
       "schema": null
      },
      {
       "status": "404",
       "description": "No organization matches the slug (ORG_NOT_FOUND) or the  organization has no sso_config (SSO_NOT_CONFIGURED).",
       "schema": "SsoError"
      },
      {
       "status": "502",
       "description": "The identity provider is unreachable or its discovery document is  invalid (SSO_UPSTREAM_ERROR). The upstream URL is never echoed.",
       "schema": "SsoError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/auth/sso/callback",
     "operationId": "completeSsoLogin",
     "summary": "Complete the OIDC login flow (returns the platform session token)",
     "description": "The IdP redirects here with `code` and `state`. The single-use state is consumed (replays and 10-minute-expired states are 401), the code is exchanged (client_secret_post; inline secret or client_secret_ref resolved from the platform secrets store), the RS256 ID token is verified end-to-end and the local identity is JIT-provisioned or linked by the email claim. Returns the platform's existing HMAC session token \u2014 the same format as POST /auth/login \u2014 plus a credential-free user view. Disabled accounts (SCIM active=false) are refused with 403 ACCOUNT_DISABLED.",
     "permission": "",
     "auth": "public",
     "params": [
      {
       "name": "code",
       "in": "query",
       "required": true,
       "type": "string",
       "description": "Authorization code issued by the IdP."
      },
      {
       "name": "state",
       "in": "query",
       "required": true,
       "type": "string",
       "description": "Opaque single-use state issued by beginSsoLogin."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Login completed; the identity is provisioned/linked.",
       "schema": "SsoLoginResponse"
      },
      {
       "status": "400",
       "description": "Missing code or state (SSO_INVALID_REQUEST).",
       "schema": "SsoError"
      },
      {
       "status": "401",
       "description": "Unknown/expired/replayed state (SSO_STATE_INVALID) or a failed  identity-provider exchange / ID-token validation  (SSO_LOGIN_FAILED).",
       "schema": "SsoError"
      },
      {
       "status": "403",
       "description": "Account deprovisioned (ACCOUNT_DISABLED).",
       "schema": "SsoError"
      },
      {
       "status": "409",
       "description": "The email belongs to another organization  (SSO_EMAIL_FOREIGN_ORG) or the account is linked to a different  IdP subject (SSO_SUBJECT_MISMATCH \u2014 potential account takeover).",
       "schema": "SsoError"
      },
      {
       "status": "502",
       "description": "Identity provider unreachable or misconfigured (SSO_UPSTREAM_ERROR).",
       "schema": "SsoError"
      }
     ]
    }
   ]
  },
  {
   "tag": "batch",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/batch/runs",
     "operationId": "createBatchRuns",
     "summary": "Run one agent over N inputs or N agents over one input (batch)",
     "description": "",
     "permission": "batch.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "BatchRunsRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Every item succeeded.",
       "schema": "BatchRunsResponse"
      },
      {
       "status": "207",
       "description": "",
       "schema": "BatchRunsResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "422",
       "description": "",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "408",
       "description": "",
       "schema": "BatchErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/batch/workflows/{id}/trigger",
     "operationId": "triggerBatchWorkflow",
     "summary": "Trigger one workflow execution per input (batch)",
     "description": "",
     "permission": "batch.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Workflow id (UUID) to trigger once per input."
      }
     ],
     "bodySchema": "BatchWorkflowTriggerRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Every item succeeded (workflow_run_ids in submission order).",
       "schema": "BatchWorkflowTriggerResponse"
      },
      {
       "status": "207",
       "description": "Partial failure \u2014 items carry per-item errors; workflow_run_ids lists succeeded executions only.",
       "schema": "BatchWorkflowTriggerResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "422",
       "description": "",
       "schema": "BatchErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/batch/agents/update",
     "operationId": "updateBatchAgents",
     "summary": "Apply one validated patch to N agents (batch)",
     "description": "",
     "permission": "batch.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "BatchAgentUpdateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Every agent patched (updated_agent_ids in submission order).",
       "schema": "BatchAgentUpdateResponse"
      },
      {
       "status": "207",
       "description": "Partial failure \u2014 unknown agents surface as failed items.",
       "schema": "BatchAgentUpdateResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "422",
       "description": "",
       "schema": "BatchErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/batch/agents/deploy",
     "operationId": "deployBatchAgents",
     "summary": "Create one deployment per agent (batch)",
     "description": "",
     "permission": "batch.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "BatchAgentDeployRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Every deployment created (deployment_ids in submission order).",
       "schema": "BatchAgentDeployResponse"
      },
      {
       "status": "207",
       "description": "Partial failure \u2014 agents without a published version surface as failed items.",
       "schema": "BatchAgentDeployResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "422",
       "description": "",
       "schema": "BatchErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/batch/runs/cancel",
     "operationId": "cancelBatchRuns",
     "summary": "Cancel N runs (batch)",
     "description": "",
     "permission": "batch.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "BatchRunCancelRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Every run cancelled (cancelled_run_ids in submission order).",
       "schema": "BatchRunCancelResponse"
      },
      {
       "status": "207",
       "description": "Partial failure \u2014 terminal runs surface as failed items.",
       "schema": "BatchRunCancelResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "422",
       "description": "",
       "schema": "BatchErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/batch/secrets",
     "operationId": "createBatchSecrets",
     "summary": "Create N encrypted secrets (batch)",
     "description": "",
     "permission": "batch.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "BatchSecretsRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Every secret created (`created` lists the secret names).",
       "schema": "BatchSecretsResponse"
      },
      {
       "status": "207",
       "description": "Partial failure \u2014 duplicate/invalid names surface as failed items.",
       "schema": "BatchSecretsResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "422",
       "description": "",
       "schema": "BatchErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/batch/{id}",
     "operationId": "getBatch",
     "summary": "Poll one batch record (per-item states, summary, timestamps)",
     "description": "",
     "permission": "batch.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Batch id (UUID, as returned by the submission response)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The batch record (any status).",
       "schema": "BatchRecord"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "404",
       "description": "NOT_FOUND \u2014 unknown id or a batch owned by another organization.",
       "schema": "BatchErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/batch/{id}/events",
     "operationId": "streamBatchEvents",
     "summary": "SSE progress stream for one batch",
     "description": "",
     "permission": "batch.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Batch id (UUID, as returned by the submission response)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "text/event-stream of batch.updated progress events.",
       "schema": null
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "404",
       "description": "NOT_FOUND \u2014 unknown id or a batch owned by another organization.",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "503",
       "description": "STREAMING_UNAVAILABLE \u2014 the event stream service is not wired.",
       "schema": "BatchErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/batch/{id}/cancel",
     "operationId": "cancelBatch",
     "summary": "Cancel a running batch (idempotent on terminal batches)",
     "description": "",
     "permission": "batch.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Batch id (UUID, as returned by the submission response)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The (cancelled) batch record.",
       "schema": "BatchRecord"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "BatchErrorEnvelope"
      },
      {
       "status": "404",
       "description": "NOT_FOUND \u2014 unknown id or a batch owned by another organization.",
       "schema": "BatchErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "billing",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/billing/margin",
     "operationId": "getBillingMargin",
     "summary": "Cost basis vs plan price for the current subscription period",
     "description": "Owner-only read (organization.manage). Computes the margin of the org's CURRENT subscription period: margin_cents = plan.price_cents \u2212 round(\u03a3 usage cost over [period_start, period_end)), with the cost basis taken from the same ledger that prices invoice run lines (runs.cost_cents aggregates \u2014 it always reconciles with GET /usage/costs and the invoice of the same window). margin_percent = (1 \u2212 cost/price) \u00d7 100 and is OMITTED when price_cents is 0. Overage pricing is not folded into revenue; unlimited plans (included_quota 0) compute identically; a cost above the price yields a negative (unclamped) margin; zero usage yields the full price (100%). For a canceled subscription the latest row's last period is reported.",
     "permission": "organization.manage",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The margin report for the current subscription period.",
       "schema": "SmMarginResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "SmErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but the role lacks the required permission (meters need  MEMBER+ via runs.execute; margin is OWNER-only via  organization.manage).",
       "schema": "SmErrorEnvelope"
      },
      {
       "status": "404",
       "description": "The organization never subscribed (error.code NO_SUBSCRIPTION) or  the subscription's plan no longer resolves (error.code  PLAN_NOT_FOUND \u2014 margin cannot exist without a price).",
       "schema": "SmErrorEnvelope"
      },
      {
       "status": "405",
       "description": "Method not allowed (the endpoint is GET-only).",
       "schema": null
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "SmErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/billing/plans",
     "operationId": "listBillingPlans",
     "summary": "List the global plan catalog",
     "description": "Read (runs.execute, MEMBER+). Returns every billable plan. Plans are global (non-tenant) catalog rows: price_cents is the monthly catalog price, included_quota is the included monthly run budget (0 = unlimited \u2014 never exceeded, never overaged) and metadata may carry pricing knobs such as {\"overage_run_rate_cents\": N}.",
     "permission": "runs.execute",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The plan catalog (possibly empty).",
       "schema": "BillPlanList"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but the role lacks the required permission (VIEWERs  lack reads; only OWNER may subscribe).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "405",
       "description": "Method not allowed (the endpoint is GET-only).",
       "schema": null
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "BillErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/billing/subscriptions",
     "operationId": "createBillingSubscription",
     "summary": "Subscribe the caller's organization to a plan",
     "description": "Owner-only write (organization.manage). Creates the organization's subscription in TRIAL state with a 14-day window [now, now+14d). Fails with 409 while a live (trial/active/past_due) subscription exists \u2014 one live subscription per organization is a schema invariant; after a cancellation a fresh subscribe opens a NEW lifecycle row (canceled rows remain readable history). The tenant is the caller's auth-claims organization; a client-supplied organization id is never trusted.",
     "permission": "organization.manage",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "BillSubscribeRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "The created subscription (trial state).",
       "schema": "BillSubscriptionResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body or missing plan_id  (error.code INVALID_REQUEST / VALIDATION_ERROR).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but the role lacks the required permission (VIEWERs  lack reads; only OWNER may subscribe).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown plan (error.code PLAN_NOT_FOUND).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "405",
       "description": "Method not allowed (the endpoint is POST-only).",
       "schema": null
      },
      {
       "status": "409",
       "description": "The organization already has a live subscription  (error.code CONFLICT).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "BillErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/billing/subscription",
     "operationId": "getBillingSubscription",
     "summary": "Show the organization's current subscription and quota state",
     "description": "Read (runs.execute, MEMBER+). Returns the org's live subscription together with its monthly run-budget quota status. When no live subscription exists the most recent one in any status is returned (canceled history stays readable); 404 NO_SUBSCRIPTION when the organization never subscribed. Quota semantics: consumed_runs counts metered runs inside the subscription's current period window, remaining_runs = max(0, included_quota - consumed_runs) and exceeded is true only when consumed_runs > included_quota (consumed == included is still within budget; unlimited plans \u2014 included_quota 0 \u2014 are never exceeded).",
     "permission": "runs.execute",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The current subscription with its quota status.",
       "schema": "BillSubscriptionWithQuota"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but the role lacks the required permission (VIEWERs  lack reads; only OWNER may subscribe).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "404",
       "description": "The organization never subscribed (error.code NO_SUBSCRIPTION).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "405",
       "description": "Method not allowed (the endpoint is GET-only).",
       "schema": null
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "BillErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/billing/subscription/cancel",
     "operationId": "cancelBillingSubscription",
     "summary": "Cancel the organization's live subscription",
     "description": "Owner/ADMIN write (users.manage). Body is optional. immediate=true cancels NOW (status=canceled, canceled_at stamped). immediate=false (the default) schedules the cancel for the end of the running period (cancel_at_period_end=true; the status flips to canceled when the period rolls over). Canceled subscriptions cannot be cancelled again (404 NO_SUBSCRIPTION once no live row exists).",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "BillCancelRequest",
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The updated subscription (canceled now, or flagged for period end).",
       "schema": "BillSubscriptionResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body (error.code INVALID_REQUEST).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but the role lacks the required permission (VIEWERs  lack reads; only OWNER may subscribe).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "404",
       "description": "No live subscription (error.code NO_SUBSCRIPTION).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "405",
       "description": "Method not allowed (the endpoint is POST-only).",
       "schema": null
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "BillErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/billing/invoices",
     "operationId": "listBillingInvoices",
     "summary": "List the organization's invoices",
     "description": "Read (runs.execute, MEMBER+). Returns the org's invoices, newest first, WITHOUT lines (use GET /billing/invoices/{id} for the full document). Every row is scoped to the caller's organization.",
     "permission": "runs.execute",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The invoice list (possibly empty).",
       "schema": "BillInvoiceList"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but the role lacks the required permission (VIEWERs  lack reads; only OWNER may subscribe).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "405",
       "description": "Method not allowed (the endpoint is GET-only).",
       "schema": null
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "BillErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/billing/invoices/{id}",
     "operationId": "getBillingInvoice",
     "summary": "Show one invoice with its lines",
     "description": "Read (runs.execute, MEMBER+). Returns the full invoice document including its lines. Tenant-scoped: an invoice of another organization is indistinguishable from an unknown id (404 INVOICE_NOT_FOUND). Line sources: run (model-bucketed usage priced from the cost aggregates), eval (evaluation metering seam) and overage (runs beyond the plan's included quota, priced by the plan metadata overage_run_rate_cents knob). subtotal_cents is always the exact sum of the line amounts.",
     "permission": "runs.execute",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Invoice identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The invoice with its lines.",
       "schema": "BillInvoiceResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but the role lacks the required permission (VIEWERs  lack reads; only OWNER may subscribe).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown or foreign invoice (error.code INVOICE_NOT_FOUND).",
       "schema": "BillErrorEnvelope"
      },
      {
       "status": "405",
       "description": "Method not allowed (the endpoint is GET-only).",
       "schema": null
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "BillErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "budgets",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/budgets",
     "operationId": "createBudget",
     "summary": "Create a budget (org or agent scope)",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [],
     "bodySchema": "BudgetInput",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Created.",
       "schema": "object"
      },
      {
       "status": "400",
       "description": "Validation error.",
       "schema": "BudgetError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BudgetError"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier.",
       "schema": "BudgetError"
      },
      {
       "status": "404",
       "description": "Unknown id or cross-tenant probe (no existence leak).",
       "schema": "BudgetError"
      },
      {
       "status": "409",
       "description": "An active budget already exists for (scope, ref).",
       "schema": "BudgetError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/budgets",
     "operationId": "listBudgets",
     "summary": "List budgets of the caller's organization",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "active",
       "in": "query",
       "required": false,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Budget list.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BudgetError"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier.",
       "schema": "BudgetError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/budgets/evaluate",
     "operationId": "evaluateBudget",
     "summary": "Dry-run a budget decision (dashboard pre-flight)",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "agent_id",
       "in": "query",
       "required": false,
       "type": "string",
       "description": ""
      },
      {
       "name": "runs",
       "in": "query",
       "required": false,
       "type": "string",
       "description": ""
      },
      {
       "name": "cost",
       "in": "query",
       "required": false,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Decision (allowed or denied with reason budget_exceeded).",
       "schema": "object"
      },
      {
       "status": "400",
       "description": "Validation error.",
       "schema": "BudgetError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BudgetError"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier.",
       "schema": "BudgetError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/budgets/{id}",
     "operationId": "getBudget",
     "summary": "Get one budget",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The budget.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BudgetError"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier.",
       "schema": "BudgetError"
      },
      {
       "status": "404",
       "description": "Unknown id or cross-tenant probe (no existence leak).",
       "schema": "BudgetError"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/budgets/{id}",
     "operationId": "updateBudget",
     "summary": "Update limits / active flag",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": "BudgetInput",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Updated.",
       "schema": "object"
      },
      {
       "status": "400",
       "description": "Validation error.",
       "schema": "BudgetError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BudgetError"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier.",
       "schema": "BudgetError"
      },
      {
       "status": "404",
       "description": "Unknown id or cross-tenant probe (no existence leak).",
       "schema": "BudgetError"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/budgets/{id}",
     "operationId": "deleteBudget",
     "summary": "Delete a budget",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "204",
       "description": "Deleted.",
       "schema": null
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "BudgetError"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier.",
       "schema": "BudgetError"
      },
      {
       "status": "404",
       "description": "Unknown id or cross-tenant probe (no existence leak).",
       "schema": "BudgetError"
      }
     ]
    }
   ]
  },
  {
   "tag": "compliance",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/compliance/controls",
     "operationId": "listComplianceControls",
     "summary": "Control catalog with framework mapping and live evidence for the caller's organization",
     "description": "",
     "permission": "audit.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Control-mapping report.",
       "schema": "ComControlsReport"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ComError"
      },
      {
       "status": "403",
       "description": "Authenticated but not permitted (audit.read is OWNER/ADMIN).",
       "schema": "ComError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/compliance/agents/{id}/technical-doc",
     "operationId": "getAgentTechnicalDoc",
     "summary": "Per-agent technical documentation (EU AI Act Annex IV style) generated from platform data",
     "description": "",
     "permission": "audit.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      },
      {
       "name": "format",
       "in": "query",
       "required": false,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "",
       "schema": "ComTechDoc"
      },
      {
       "status": "400",
       "description": "Invalid request (format must be json|md; window from/to required, RFC3339 or YYYY-MM-DD, from <= to).",
       "schema": "ComError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ComError"
      },
      {
       "status": "403",
       "description": "Authenticated but not permitted (audit.read is OWNER/ADMIN).",
       "schema": "ComError"
      },
      {
       "status": "404",
       "description": "Unknown or foreign agent \u2014 no existence leak.",
       "schema": "ComError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/compliance/evidence",
     "operationId": "exportComplianceEvidence",
     "summary": "Deterministic JSONL compliance evidence bundle for a fixed window",
     "description": "",
     "permission": "audit.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "from",
       "in": "query",
       "required": true,
       "type": "string",
       "description": ""
      },
      {
       "name": "to",
       "in": "query",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "NDJSON evidence bundle.",
       "schema": null
      },
      {
       "status": "400",
       "description": "Invalid request (format must be json|md; window from/to required, RFC3339 or YYYY-MM-DD, from <= to).",
       "schema": "ComError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ComError"
      },
      {
       "status": "403",
       "description": "Authenticated but not permitted (audit.read is OWNER/ADMIN).",
       "schema": "ComError"
      }
     ]
    }
   ]
  },
  {
   "tag": "connectors",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/connectors",
     "operationId": "createConnector",
     "summary": "Register one org-scoped external integration",
     "description": "Write operation (connectors.write, OWNER/ADMIN). The body carries the connector name (unique per organization), type (webhook|http), base_url (absolute http(s) URL), config (auth_style plus optional header templates and api-key/basic parameters) and an optional secret_ref NAME referencing a secret in the secrets store. Secret values are never accepted by this endpoint. Status defaults to active. Duplicate (org, name) is a 409. The tenant is derived from the auth claims \u2014 any client-supplied organization id is ignored.",
     "permission": "connectors.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "ConCreateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Connector registered.",
       "schema": "ConResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body (error.code INVALID_REQUEST).",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "409",
       "description": "A connector with the same name already exists in the caller's organization (error.code CONNECTOR_ALREADY_EXISTS).",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "422",
       "description": "Validation error: missing name/base_url, name over 255 chars, type  not webhook|http, base_url not an absolute http(s) URL, status not  active|disabled, auth_style not one of none|bearer|basic|  api_key_header, or a header template containing CR/LF  (error.code VALIDATION_ERROR).",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ConErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/connectors",
     "operationId": "listConnectors",
     "summary": "List connectors for the caller's organization",
     "description": "Read operation (connectors.read, MEMBER+ \u2014 viewers are excluded because connector listings expose integration topology). Returns all connectors of the caller's organization ordered by name ASC; each entry is the value-free projection (config templates + secret_ref name only, never secret values).",
     "permission": "connectors.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Connector list (possibly empty).",
       "schema": "ConListResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ConErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/connectors/{id}",
     "operationId": "getConnector",
     "summary": "Fetch one connector",
     "description": "Read operation (connectors.read, MEMBER+). Returns the value-free projection of one connector including the recorded health-check bookkeeping (last_check_at / last_check_status, null/\"\" when never probed). Foreign or unknown ids surface as 404 (no existence leak across tenants).",
     "permission": "connectors.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Connector identifier (unique per organization)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Connector projection.",
       "schema": "ConResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Connector not found within the caller's organization (error.code CONNECTOR_NOT_FOUND).",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ConErrorEnvelope"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/connectors/{id}",
     "operationId": "deleteConnector",
     "summary": "Hard-delete one connector",
     "description": "Write operation (connectors.write, OWNER/ADMIN). Removes the registry entry (connectors are hard-deleted; the unique (org, name) constraint frees the name immediately). Foreign or unknown ids are 404. Deletion is audit-logged.",
     "permission": "connectors.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Connector identifier (unique per organization)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Connector deleted.",
       "schema": "ConDeleteResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Connector not found within the caller's organization (error.code CONNECTOR_NOT_FOUND).",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ConErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/connectors/{id}/test",
     "operationId": "testConnector",
     "summary": "Run the live health check for one connector",
     "description": "Write operation (connectors.write, OWNER/ADMIN \u2014 the probe triggers outbound network activity and mutates connector state). Performs a GET to the connector's base_url with the configured auth style applied (the secret is resolved server-side through the secrets store and injected into the outgoing request only \u2014 it never appears here), bounded by a 5s timeout. Classifies the outcome as ok (2xx-3xx) or error (non-2xx, network failure, timeout, unresolvable secret), records last_check_at / last_check_status on the connector and returns the probe result. Disabled connectors remain probeable \u2014 the check exists precisely to inform the enable/disable decision.",
     "permission": "connectors.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Connector identifier (unique per organization)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Probe outcome (never includes secret values).",
       "schema": "ConTestResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Connector not found within the caller's organization (error.code CONNECTOR_NOT_FOUND).",
       "schema": "ConErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (e.g. persistence of the probe outcome failed).",
       "schema": "ConErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "deployments",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/deployments",
     "operationId": "listDeployments",
     "summary": "List deployments, optionally filtered by agent",
     "description": "",
     "permission": "deployments.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "agent_id",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Restrict the listing to one agent."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deployments of the tenant (newest first).",
       "schema": "DeploymentsResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/deployments/create",
     "operationId": "createDeployment",
     "summary": "Create a deployment (status requested) of a published agent version",
     "description": "",
     "permission": "deployments.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "CreateDeploymentRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Deployment created in status `requested`; advance it with promote.",
       "schema": "DeploymentResponse"
      },
      {
       "status": "400",
       "description": "Malformed request body.",
       "schema": "ErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "422",
       "description": "Validation failed (VALIDATION_ERROR for bad environment/fields;  VERSION_NOT_PUBLISHED when the target version does not exist or is  not published).",
       "schema": "ErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/deployments/{id}",
     "operationId": "getDeployment",
     "summary": "Fetch one deployment",
     "description": "",
     "permission": "deployments.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Deployment identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The deployment.",
       "schema": "DeploymentResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/deployments/{id}/promote",
     "operationId": "promoteDeployment",
     "summary": "Advance the lifecycle one step (requested -> validated -> deploying -> healthy)",
     "description": "",
     "permission": "deployments.deploy",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Deployment identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deployment advanced one step (a worker replaces explicit promote later).",
       "schema": "DeploymentResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      },
      {
       "status": "409",
       "description": "Terminal status (healthy/failed) cannot advance.",
       "schema": "ErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/deployments/{id}/rollback",
     "operationId": "rollbackDeployment",
     "summary": "Re-point the environment to the previous healthy deployment's version",
     "description": "",
     "permission": "deployments.deploy",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Deployment identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Environment re-pointed; response carries the new healthy deployment and the version it serves.",
       "schema": "RollbackDeploymentResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      },
      {
       "status": "409",
       "description": "No previous healthy deployment exists for this agent+environment.",
       "schema": "ErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/deployments/{id}/canary",
     "operationId": "setDeploymentCanary",
     "summary": "Attach or replace the canary version and/or move the traffic split",
     "description": "Restatement of the issue-#13 operation with the issue-#51 addition: the body gains the optional `canary_policy` object (the eval-gated promotion policy; `null` clears a configured policy). Attaching or replacing a canary version opens a FRESH evaluation window and voids any prior automatic decision; the policy itself is a standing rule of the row and survives attach/replace. At least one body field is required; a canary_policy-only body requires an already-attached canary (409 otherwise). All other semantics \u2014 existing versions only, weight 0-100 rejected-not-clamped, healthy rows only \u2014 are unchanged (see api/fragments/canary.yaml).",
     "permission": "deployments.deploy",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Deployment identifier (UUID v4 string)."
      }
     ],
     "bodySchema": "CnpCanaryConfigRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Updated deployment (canary fields + promotion state included).",
       "schema": "CnpDeploymentResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body (error.code INVALID_REQUEST).",
       "schema": "CnpErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "CnpErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission (writes are deployments.deploy \u2014 OWNER/ADMIN; the status read is runs.read \u2014 all roles).",
       "schema": "CnpErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Deployment not found within the caller's organization (error.code DEPLOYMENT_NOT_FOUND).",
       "schema": "CnpErrorEnvelope"
      },
      {
       "status": "409",
       "description": "State conflict: canary_policy-only body without an attached canary,  weight moved without a canary, or the row is not the environment's  healthy deployment (error.code INVALID_STATE).",
       "schema": "CnpErrorEnvelope"
      },
      {
       "status": "422",
       "description": "Validation error: empty body, out-of-domain canary_policy values  (min_pass_rate outside [0,1] or a negative threshold \u2014 rejected,  not clamped), canary_weight outside 0-100, unknown/cross-agent  canary version (error.code VALIDATION_ERROR).",
       "schema": "CnpErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "CnpErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/agents/{agentId}/canary/status",
     "operationId": "getCanaryStatus",
     "summary": "Read the eval-gated canary promotion state for one agent",
     "description": "Additive read-only endpoint (issue #51; runs.read \u2014 all roles). Serves the operator's view for one agent+environment: the current traffic split, the promotion policy in effect (a standing rule that survives abort and re-attach), the last automatic decision with its reason and sample stats, and \u2014 while the canary is ACTIVE \u2014 fresh statistics of the completed eval runs inside the window (runs counted, case-weighted pass rate, nearest-rank p95 latency, average cost per run). With no sample source wired (or no active canary) `stats` is omitted; with no configured policy `policy`/`window_start`/`decision` are omitted. The tenant comes from the auth claims; foreign agents surface as 404 (no existence leak).",
     "permission": "runs.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "agentId",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Agent identifier (UUID v4 string)."
      },
      {
       "name": "environment",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Target environment of the canary (default: production). One of  development|staging|production.  "
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The canary promotion status view.",
       "schema": "CnpCanaryStatusResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "CnpErrorEnvelope"
      },
      {
       "status": "404",
       "description": "No healthy deployment serves this agent+environment for the  caller's organization, or the agent is unknown/foreign  (error.code NOT_FOUND).",
       "schema": "CnpErrorEnvelope"
      },
      {
       "status": "405",
       "description": "Method not allowed (the endpoint is GET-only).",
       "schema": null
      },
      {
       "status": "422",
       "description": "Unknown `environment` query value, error.code VALIDATION_ERROR (valid values development|staging|production).",
       "schema": "CnpErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "CnpErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/deployments/{id}/canary/promote",
     "operationId": "promoteCanaryDeployment",
     "summary": "Make the canary the stable version (weight -> 0, config cleared)",
     "description": "Traffic-changing operation (deployments.deploy, OWNER/ADMIN). The canary becomes the stable version: the deployment's `version` swaps to the canary version and the canary config is cleared (canary_version 0, canary_weight 0). The row STAYS healthy \u2014 no lifecycle transition, no new ledger row \u2014 so the one-healthy-per-agent+environment invariant holds and history remains append-only. Rejected when the deployment has no canary or is not healthy (INVALID_STATE).",
     "permission": "deployments.deploy",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Deployment identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The promotion result (stable version swapped, canary cleared).",
       "schema": "CanaryDeploymentResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "CanaryErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the deployments.deploy permission (OWNER/ADMIN only).",
       "schema": "CanaryErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Deployment not found within the caller's organization (error.code DEPLOYMENT_NOT_FOUND).",
       "schema": "CanaryErrorEnvelope"
      },
      {
       "status": "409",
       "description": "No canary configured or deployment not healthy (error.code INVALID_STATE).",
       "schema": "CanaryErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "CanaryErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/deployments/{id}/canary/abort",
     "operationId": "abortCanaryDeployment",
     "summary": "Clear the canary and keep the stable version serving 100%",
     "description": "Traffic-changing operation (deployments.deploy, OWNER/ADMIN). Clears the canary config (canary_version 0, canary_weight 0); the stable version keeps serving all traffic. Rejected when the deployment has no canary or is not healthy (INVALID_STATE).",
     "permission": "deployments.deploy",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Deployment identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The aborted deployment (stable version unchanged, canary cleared).",
       "schema": "CanaryDeploymentResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "CanaryErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the deployments.deploy permission (OWNER/ADMIN only).",
       "schema": "CanaryErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Deployment not found within the caller's organization (error.code DEPLOYMENT_NOT_FOUND).",
       "schema": "CanaryErrorEnvelope"
      },
      {
       "status": "409",
       "description": "No canary configured or deployment not healthy (error.code INVALID_STATE).",
       "schema": "CanaryErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "CanaryErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "Evaluations",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/eval-datasets",
     "operationId": "listEvalDatasets",
     "summary": "List evaluation datasets (tenant-scoped)",
     "description": "",
     "permission": "",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Datasets of the caller's organization, newest first.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/eval-datasets/create",
     "operationId": "createEvalDataset",
     "summary": "Create an evaluation dataset with its cases",
     "description": "Validates every case before persisting: supported scorers are `exact`, `contains`, `regex`, `latency_under_ms`, `cost_under_cents`; `regex` requires `params.pattern` (compilable), `latency_under_ms` requires `params.threshold_ms > 0`, `cost_under_cents` requires `params.threshold_cents > 0`; case ids must be unique (auto-generated when omitted); at most 50 cases per dataset.",
     "permission": "",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "object",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Created dataset (echoes the ordered case list).",
       "schema": "object"
      },
      {
       "status": "400",
       "description": "Validation error (unknown scorer, bad params, duplicate case ids, >50 cases).",
       "schema": "EvalErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/eval-datasets/{id}",
     "operationId": "getEvalDataset",
     "summary": "Get one evaluation dataset including its cases",
     "description": "",
     "permission": "",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Dataset with cases.",
       "schema": "object"
      },
      {
       "status": "404",
       "description": "Dataset not found in the caller's organization.",
       "schema": "EvalErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/eval-datasets/{id}/run",
     "operationId": "runEvalDataset",
     "summary": "Execute every case of a dataset against an agent (synchronous, bounded)",
     "description": "Runs each case through the agent runtime with a per-case context deadline (30s) and at most 50 cases. Returns the completed run id; fetch `GET /eval-runs/{id}` for results and summary. Cost is recorded as 0 until the runtime exposes cost (token usage only today).",
     "permission": "",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": "object",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Run finished; status is `completed` (per-case failures land in the results).",
       "schema": "object"
      },
      {
       "status": "400",
       "description": "Missing agent_id or validation error.",
       "schema": "EvalErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Dataset or agent not found in the caller's organization.",
       "schema": "EvalErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "503",
       "description": "Evals runner not wired into the API server.",
       "schema": "EvalErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/eval-runs/compare",
     "operationId": "compareEvalRuns",
     "summary": "Compare a candidate run against a baseline run",
     "description": "Cases are matched by case_id; only the intersection of both runs is judged. A regression is a case the baseline passed and the candidate failed; an improvement is the reverse.",
     "permission": "",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "object",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Comparison with both summaries and case-level deltas.",
       "schema": "EvalComparison"
      },
      {
       "status": "400",
       "description": "Missing run ids.",
       "schema": "EvalErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Baseline or candidate run not found in the caller's organization.",
       "schema": "EvalErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/eval-runs/{id}",
     "operationId": "getEvalRun",
     "summary": "Get one evaluation run with per-case results and summary",
     "description": "",
     "permission": "",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Run with results and aggregate summary.",
       "schema": "EvalRunResponse"
      },
      {
       "status": "404",
       "description": "Run not found in the caller's organization.",
       "schema": "EvalErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      }
     ]
    }
   ]
  },
  {
   "tag": "events",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/events",
     "operationId": "listEvents",
     "summary": "List the org's domain event stream (activity feed)",
     "description": "Returns one page of the caller's organization's events, newest first, with an opaque next_cursor for keyset continuation. Use `limit` to size the page (clamped server-side to [1, 200]; default 50) and pass `cursor` back unchanged to fetch the next page. Filters narrow the stream to one event `type` and/or one entity (`entity_type` + `entity_id`) and/or everything after `since` (RFC3339, inclusive). Requires MEMBER+ (runs.execute): VIEWER roles are denied 403.",
     "permission": "runs.execute (MEMBER+ \u2014 OWNER/ADMIN/MEMBER; VIEWER denied)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "limit",
       "in": "query",
       "required": false,
       "type": "integer",
       "description": "Page size (clamped server-side; values <= 0 are a 400)."
      },
      {
       "name": "cursor",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Opaque keyset cursor from the previous response's next_cursor. Malformed cursors answer 400 INVALID_CURSOR."
      },
      {
       "name": "type",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Exact contract event type, e.g. run.failed, agent.created. Unknown types answer 400 INVALID_REQUEST (the vocabulary is pinned)."
      },
      {
       "name": "entity_type",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Exact resource_type match (the domain object the event is about, e.g. \"run\", \"agent\", \"webhook\")."
      },
      {
       "name": "entity_id",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Exact resource_id match."
      },
      {
       "name": "since",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "RFC3339 inclusive lower bound on the event timestamp."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "One keyset page of events (newest first).",
       "schema": "EvEventListResponse"
      },
      {
       "status": "400",
       "description": "Invalid query parameter (error.code INVALID_REQUEST for a non-positive/non-integer limit, unknown event type or malformed since; INVALID_CURSOR for a malformed/stale cursor).",
       "schema": "EvErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "EvErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but not permitted (VIEWER has no runs.execute grant)",
       "schema": "EvErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "EvErrorEnvelope"
      },
      {
       "status": "503",
       "description": "Event retention is not wired in this deployment (error.code EVENTS_UNAVAILABLE).",
       "schema": "EvErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "gateway",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/gateway/keys",
     "operationId": "createGatewayKey",
     "summary": "Mint a virtual key (plaintext vk_\u2026 returned exactly once)",
     "description": "",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "GatewayKeyCreateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "",
       "schema": "GatewayKeyCreatedResponse"
      },
      {
       "status": "400",
       "description": "",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier (gateway surface is OWNER/ADMIN).",
       "schema": "GatewayErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/gateway/keys",
     "operationId": "listGatewayKeys",
     "summary": "List virtual keys of the caller's organization (no hashes, no secrets)",
     "description": "",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Key list (possibly empty; structurally secret-free).",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier (gateway surface is OWNER/ADMIN).",
       "schema": "GatewayErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/gateway/keys/{id}",
     "operationId": "getGatewayKey",
     "summary": "Get one virtual key (redacted view)",
     "description": "",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The key.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier (gateway surface is OWNER/ADMIN).",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown id or cross-tenant probe (no existence leak).",
       "schema": "GatewayErrorEnvelope"
      }
     ]
    },
    {
     "method": "PATCH",
     "path": "/gateway/keys/{id}",
     "operationId": "updateGatewayKey",
     "summary": "Update label / allow-list / monthly limit / expiry",
     "description": "",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": "GatewayKeyUpdateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Updated key (redacted view).",
       "schema": "object"
      },
      {
       "status": "400",
       "description": "Validation error (error.code VALIDATION_ERROR).",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier (gateway surface is OWNER/ADMIN).",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown id or cross-tenant probe (no existence leak).",
       "schema": "GatewayErrorEnvelope"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/gateway/keys/{id}",
     "operationId": "revokeGatewayKey",
     "summary": "Revoke a virtual key (idempotent, terminal)",
     "description": "",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "204",
       "description": "Revoked (or already revoked).",
       "schema": null
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier (gateway surface is OWNER/ADMIN).",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown id or cross-tenant probe (no existence leak).",
       "schema": "GatewayErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/gateway/keys/{id}/spend",
     "operationId": "getGatewayKeySpend",
     "summary": "Month-to-date spend view for one virtual key",
     "description": "",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      },
      {
       "name": "month",
       "in": "query",
       "required": false,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Spend view for the requested month.",
       "schema": "GatewaySpendView"
      },
      {
       "status": "400",
       "description": "Malformed month parameter (error.code VALIDATION_ERROR).",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier (gateway surface is OWNER/ADMIN).",
       "schema": "GatewayErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown id or cross-tenant probe (no existence leak).",
       "schema": "GatewayErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "Governance",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/policies",
     "operationId": "listPolicies",
     "summary": "List policies of the authenticated organization",
     "description": "Returns the governance policies of the caller's organization ordered by priority (desc) then creation time (asc) \u2014 the same order the evaluation engine applies. Unpaginated (see base spec limitation).",
     "permission": "policies.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Policy list (possibly empty)",
       "schema": "PolicyListResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "500",
       "description": "Unexpected server error",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/policies/create",
     "operationId": "createPolicy",
     "summary": "Create a policy",
     "description": "Validates the policy record (name required; effect allow|deny; resource_type tool|agent|workflow|deployment|*; no empty entries in actions / tool_allowlist / environments; max_cost_cents >= 0) and persists it for the caller's organization. id and timestamps are assigned server-side. Returns 422 with an invalid_policy error when validation fails.",
     "permission": "policies.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "Policy",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Created policy",
       "schema": "PolicyResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body",
       "schema": "Error"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "422",
       "description": "Policy validation failed",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/policies/evaluate",
     "operationId": "evaluatePolicies",
     "summary": "Evaluate the caller's policies against one action",
     "description": "Runs the governance engine over the enabled policies of the caller'snorganization: highest-priority match wins, deny beats allow on anpriority tie, and the default decision is allow when nothing matches.nCondition semantics (conditions are applicability predicates):n * tool_allowlist (non-empty) \u2014 the request tool must be listed;n * environments (non-empty) \u2014 the request environment must be listed;n * max_cost_cents (set >= 0) \u2014 the request is over budget, i.e.n estimated_cost_cents > max_cost_cents (budget-guard);n * require_approval \u2014 annotated in the decision reason.nThe tenant scope always comes from the authenticated principal;nresource.tenant_id is informational. Evaluate is a read-only operationnand requires policies.read.n",
     "permission": "policies.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "PolicyEvaluateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Decision",
       "schema": "PolicyEvaluateResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body",
       "schema": "Error"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "422",
       "description": "Missing action",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/policies/{id}",
     "operationId": "getPolicy",
     "summary": "Get one policy",
     "description": "",
     "permission": "policies.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Policy id (UUID string)"
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The policy",
       "schema": "PolicyResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/policies/{id}",
     "operationId": "updatePolicy",
     "summary": "Full update of one policy",
     "description": "Replaces the mutable fields (name, effect, resource_type, actions, conditions, priority, enabled). id, organization_id and created_at are preserved; updated_at is bumped. Validates like create.",
     "permission": "policies.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Policy id (UUID string)"
      }
     ],
     "bodySchema": "Policy",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Updated policy",
       "schema": "PolicyResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body",
       "schema": "Error"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      },
      {
       "status": "422",
       "description": "Policy validation failed",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/policies/{id}",
     "operationId": "deletePolicy",
     "summary": "Delete one policy",
     "description": "",
     "permission": "policies.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Policy id (UUID string)"
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deletion confirmation",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      }
     ]
    }
   ]
  },
  {
   "tag": "Knowledge",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/knowledge/documents",
     "operationId": "listKnowledgeDocuments",
     "summary": "List the caller's knowledge documents (newest first)",
     "description": "",
     "permission": "agents.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Documents of the caller's organization, newest first.",
       "schema": "KnbDocumentsResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "KnbErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/knowledge/documents",
     "operationId": "createKnowledgeDocument",
     "summary": "Ingest a document (chunk ~800 chars / 15% overlap -> embed -> store)",
     "description": "Runs the ingestion pipeline: the content is chunked on boundaries (~800 chars, ~15% overlap), each chunk is embedded through the OpenAI-compatible embeddings backend, and document + chunks are stored atomically. An embeddings-backend failure is NON-FATAL: the response is still 201 with an additive `warning` field, and the unembedded chunks stay searchable via lexical scoring.",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "KnbCreateDocumentRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Document ingested (echoes the stored document row).",
       "schema": "KnbCreateDocumentResponse"
      },
      {
       "status": "400",
       "description": "Malformed request body.",
       "schema": "KnbErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "422",
       "description": "Validation failed (title/content required).",
       "schema": "KnbErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (e.g. store unavailable).",
       "schema": "KnbErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/knowledge/search",
     "operationId": "searchKnowledge",
     "summary": "Top-k chunk retrieval (keyword | semantic | hybrid)",
     "description": "",
     "permission": "agents.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "EmbSearchRequest",
     "bodyRequired": true,
     "bodyExample": "{\n  \"query\": \"How do budgets roll over at period end?\",\n  \"k\": 5,\n  \"mode\": \"hybrid\",\n  \"weights\": {\n    \"keyword_weight\": 0.5,\n    \"vector_weight\": 0.5,\n    \"fusion\": \"weights\"\n  }\n}",
     "responses": [
      {
       "status": "200",
       "description": "Top-k chunk hits; empty `results` when nothing matches.",
       "schema": "EmbSearchResponse"
      },
      {
       "status": "400",
       "description": "Malformed request body.",
       "schema": "EmbErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "EmbErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "EmbErrorEnvelope"
      },
      {
       "status": "422",
       "description": "Validation failed (query required, unknown mode).",
       "schema": "EmbErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (e.g. store unavailable).",
       "schema": "EmbErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "marketplace",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/marketplace/keys",
     "operationId": "registerMarketplaceSigningKey",
     "summary": "Register a publisher signing key (public half)",
     "description": "Registers the PUBLIC half of an Ed25519 keypair for the caller's organization. The private half NEVER leaves the publisher's client: sign the canonical manifest bytes locally (detached signature, base64) and pass it on publish. Duplicate (organization, public_key) pairs are rejected with 409. Audited (marketplace.key_registered, no key material in the audit row beyond the fingerprint).",
     "permission": "agents.write (OWNER/ADMIN)",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "MktSigRegisterRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Key registered",
       "schema": "MktSigKeyEnvelope"
      },
      {
       "status": "400",
       "description": "Invalid request body or key material",
       "schema": "MktSigError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "MktSigError"
      },
      {
       "status": "403",
       "description": "Authenticated principal lacks agents.write",
       "schema": "MktSigError"
      },
      {
       "status": "409",
       "description": "The org already registered this public key",
       "schema": "MktSigError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/marketplace/keys",
     "operationId": "listMarketplaceSigningKeys",
     "summary": "List the caller org's registered signing keys",
     "description": "Org-scoped listing, oldest first. Foreign keys are never visible. Revoked keys remain listed (status \"revoked\") for auditability; they cannot sign new publishes and block installs of listings they signed.",
     "permission": "agents.read (all roles)",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Keys (possibly empty)",
       "schema": "MktSigKeyListEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "MktSigError"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/marketplace/keys/{keyId}",
     "operationId": "revokeMarketplaceSigningKey",
     "summary": "Revoke a publisher signing key (idempotent)",
     "description": "Flips the key to revoked. Future publishes referencing it fail and installs of listings signed by it are blocked (unknown_signing_key); agents already installed are unaffected. Unknown AND foreign ids are the same 404 (no existence leak). Audited (marketplace.key_revoked).",
     "permission": "agents.write (OWNER/ADMIN)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "keyId",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Revocation applied (idempotent)",
       "schema": "MktSigRevokedEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "MktSigError"
      },
      {
       "status": "403",
       "description": "Authenticated principal lacks agents.write",
       "schema": "MktSigError"
      },
      {
       "status": "404",
       "description": "Unknown or foreign key id",
       "schema": "MktSigError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/marketplace/listings/{slug}/provenance",
     "operationId": "getMarketplaceListingProvenance",
     "summary": "Verifiable origin document of a listing",
     "description": "Returns the provenance record of a listing: publisher org, signing key id and fingerprint, algorithm, signed-at timestamp and the content-addressed manifest hash recorded at publish time. Visibility follows the listing's rules (published = global read; draft/unlisted = publisher org only). Unsigned/legacy listings read back with signed=false and empty provenance fields.",
     "permission": "agents.read (all roles)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "slug",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Provenance document",
       "schema": "MktSigProvenanceEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "MktSigError"
      },
      {
       "status": "404",
       "description": "Unknown slug (or not visible to the caller's org)",
       "schema": "MktSigError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/marketplace/settings",
     "operationId": "getMarketplaceSettings",
     "summary": "Read the caller org's marketplace install policy",
     "description": "Returns the org-level install policy. allow_unsigned defaults to false \u2014 installs of unsigned new listings are blocked (default: verify). Legacy listings created before signing shipped are always installable (grandfathered).",
     "permission": "agents.read (all roles)",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Org install policy",
       "schema": "MktSigSettingsEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "MktSigError"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/marketplace/settings",
     "operationId": "setMarketplaceSettings",
     "summary": "Update the caller org's marketplace install policy",
     "description": "Sets allow_unsigned for the caller's organization. true permits installs of unsigned NEW listings into this org; false (default) requires a valid publisher signature. Audited.",
     "permission": "agents.write (OWNER/ADMIN)",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "MktSigSettingsUpdate",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Updated policy",
       "schema": "MktSigSettingsEnvelope"
      },
      {
       "status": "400",
       "description": "Missing allow_unsigned (boolean required)",
       "schema": "MktSigError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "MktSigError"
      },
      {
       "status": "403",
       "description": "Authenticated principal lacks agents.write",
       "schema": "MktSigError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/marketplace/listings",
     "operationId": "publishMarketplaceListing",
     "summary": "Publish an agent as a marketplace listing",
     "description": "Creates a listing from an existing agent in the caller's organization, snapshotting the agent's current configuration (config only \u2014 never secrets). Requires OWNER/ADMIN of the publishing organization.",
     "permission": "agents.write (OWNER/ADMIN)",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "MktPublishRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Listing created (status published)",
       "schema": "MktListingEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "MktError"
      },
      {
       "status": "403",
       "description": "Authenticated but not permitted for this operation",
       "schema": "MktError"
      },
      {
       "status": "409",
       "description": "Slug already exists",
       "schema": "MktError"
      },
      {
       "status": "422",
       "description": "Validation error (unknown agent, empty name, ...)",
       "schema": "MktError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/marketplace/listings",
     "operationId": "browseMarketplaceListings",
     "summary": "Browse / search published listings",
     "description": "Global read across organizations. Supports text search over name and description (`q`), tag filtering (`tag`, repeatable) and keyset pagination (`limit` + `cursor` from `next_cursor`).",
     "permission": "authenticated",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "q",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Text match over listing name and description."
      },
      {
       "name": "tag",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Filter by tag (repeatable; OR semantics)."
      },
      {
       "name": "limit",
       "in": "query",
       "required": false,
       "type": "integer",
       "description": ""
      },
      {
       "name": "cursor",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Keyset cursor from a previous response's next_cursor."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Paginated listings page",
       "schema": "MktListingPage"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "MktError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/marketplace/listings/{slug}",
     "operationId": "getMarketplaceListing",
     "summary": "Get a single listing by slug",
     "description": "",
     "permission": "authenticated",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "slug",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The listing",
       "schema": "MktListingEnvelope"
      },
      {
       "status": "404",
       "description": "Listing not found",
       "schema": "MktError"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/marketplace/listings/{slug}",
     "operationId": "unlistMarketplaceListing",
     "summary": "Unlist a listing (publisher org OWNER/ADMIN only)",
     "description": "",
     "permission": "agents.write (publisher OWNER/ADMIN)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "slug",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Listing unlisted",
       "schema": "MktListingEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but not permitted for this operation",
       "schema": "MktError"
      },
      {
       "status": "404",
       "description": "Listing not found",
       "schema": "MktError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/marketplace/listings/{slug}/install",
     "operationId": "installMarketplaceListing",
     "summary": "Install a listing as a new agent in the caller's organization",
     "description": "Creates a NEW agent in the caller's organization from the listing's version snapshot. Name collisions get a deterministic `-2`/`-3`/... suffix. Increments the listing's download counter and writes an audit entry.",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "slug",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Agent created from the snapshot",
       "schema": "MktInstallResponse"
      },
      {
       "status": "404",
       "description": "Listing not found",
       "schema": "MktError"
      },
      {
       "status": "422",
       "description": "Snapshot no longer installable",
       "schema": "MktError"
      }
     ]
    }
   ]
  },
  {
   "tag": "mcp",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/mcp",
     "operationId": "invokeMcpEndpoint",
     "summary": "MCP JSON-RPC 2.0 endpoint (initialize / tools/list / tools/call / ping)",
     "description": "Single stateless JSON-RPC 2.0 endpoint. Exactly one request object per HTTP request; batches are rejected with -32600. Notifications (requests without an id) answer 204 No Content with no body. Method-level authorization is applied inside the endpoint: initialize and ping need only authentication, while tools/list and tools/call additionally require the runs.execute permission (OWNER/ADMIN/MEMBER). Viewers receive the in-band JSON-RPC error -32000 (HTTP 200) instead of an HTTP 403, so MCP clients surface a protocol-level error. tools/call executes through the existing internal/tools execution path \u2014 the same registry the agent runtime uses, so tool-side validation (e.g. the calculator's expression grammar, the http_request tool's SSRF protection) applies natively. Results are mapped to MCP content blocks: [{\"type\":\"text\",\"text\":\"<json or error>\"}].",
     "permission": "runs.execute",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "McpJSONRPCRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "JSON-RPC response (success result, or an in-band error: -32601  method not found, -32602 invalid params / unknown tool, -32603  internal, -32000 forbidden for the tool surface).",
       "schema": "McpJSONRPCResponse"
      },
      {
       "status": "204",
       "description": "Notification accepted (request without id): no response body by  the JSON-RPC spec.",
       "schema": null
      },
      {
       "status": "400",
       "description": "Transport-level JSON-RPC rejection: -32700 parse error or  -32600 invalid request (including batch bodies). The body is  still a JSON-RPC error envelope with id null.",
       "schema": "McpJSONRPCResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials. The platform auth middleware  answers a plain-text 401 (no JSON-RPC envelope), consistent  with every other authenticated route.",
       "schema": null
      },
      {
       "status": "413",
       "description": "Request body exceeds the 1 MiB cap; body is a JSON-RPC error  envelope (-32600) with id null.",
       "schema": "McpJSONRPCResponse"
      }
     ]
    }
   ]
  },
  {
   "tag": "mcp-servers",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/mcp-servers",
     "operationId": "registerMCPServer",
     "summary": "Register an external MCP server (health-checked)",
     "description": "",
     "permission": "agents.write (OWNER/ADMIN)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "force",
       "in": "query",
       "required": false,
       "type": "boolean",
       "description": "Register even when the health check fails (status \"unreachable\")."
      }
     ],
     "bodySchema": "McpRegServerInput",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Server registered and reachable (tools cached)",
       "schema": "McpRegServerEnvelope"
      },
      {
       "status": "400",
       "description": "Invalid request body",
       "schema": "McpRegError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "McpRegError"
      },
      {
       "status": "403",
       "description": "Authenticated principal lacks the required permission",
       "schema": "McpRegError"
      },
      {
       "status": "409",
       "description": "A server with this name already exists in the org",
       "schema": "McpRegError"
      },
      {
       "status": "422",
       "description": "Unreachable endpoint (without force) or validation error",
       "schema": "McpRegError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/mcp-servers",
     "operationId": "listMCPServers",
     "summary": "List the caller org's registered MCP servers",
     "description": "",
     "permission": "agents.read (all roles)",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Servers ordered by name (possibly empty)",
       "schema": "McpRegServerListEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "McpRegError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/mcp-servers/{id}",
     "operationId": "getMCPServer",
     "summary": "Get one registered MCP server",
     "description": "Unknown and foreign-tenant ids are the same 404 (no existence leak).",
     "permission": "agents.read (all roles)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "MCP server id (UUID, as returned at registration)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The server record (incl. cached tool catalog)",
       "schema": "McpRegServerEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "McpRegError"
      },
      {
       "status": "404",
       "description": "Unknown or foreign id",
       "schema": "McpRegError"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/mcp-servers/{id}",
     "operationId": "updateMCPServer",
     "summary": "Update an MCP server (re-checked)",
     "description": "",
     "permission": "agents.write (OWNER/ADMIN)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "MCP server id (UUID, as returned at registration)."
      },
      {
       "name": "force",
       "in": "query",
       "required": false,
       "type": "boolean",
       "description": ""
      }
     ],
     "bodySchema": "McpRegServerInput",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Updated record (post re-check)",
       "schema": "McpRegServerEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "McpRegError"
      },
      {
       "status": "403",
       "description": "Authenticated principal lacks the required permission",
       "schema": "McpRegError"
      },
      {
       "status": "404",
       "description": "Unknown or foreign id",
       "schema": "McpRegError"
      },
      {
       "status": "422",
       "description": "Unreachable endpoint (without force) or validation error",
       "schema": "McpRegError"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/mcp-servers/{id}",
     "operationId": "deleteMCPServer",
     "summary": "Delete an MCP server",
     "description": "",
     "permission": "agents.write (OWNER/ADMIN)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "MCP server id (UUID, as returned at registration)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deleted",
       "schema": "McpRegDeletedEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "McpRegError"
      },
      {
       "status": "403",
       "description": "Authenticated principal lacks the required permission",
       "schema": "McpRegError"
      },
      {
       "status": "404",
       "description": "Unknown or foreign id",
       "schema": "McpRegError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/mcp-servers/{id}/test",
     "operationId": "testMCPServer",
     "summary": "Re-run the health check and refresh the tool cache",
     "description": "",
     "permission": "agents.write (OWNER/ADMIN)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "MCP server id (UUID, as returned at registration)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Post-check record",
       "schema": "McpRegServerEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials",
       "schema": "McpRegError"
      },
      {
       "status": "403",
       "description": "Authenticated principal lacks the required permission",
       "schema": "McpRegError"
      },
      {
       "status": "404",
       "description": "Unknown or foreign id",
       "schema": "McpRegError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/mcp-servers/{id}/drift",
     "operationId": "getMcpServerDrift",
     "summary": "Drift history + current/pinned toolset hashes",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Trust state.",
       "schema": "McpTrustState"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "McpTrustError"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier.",
       "schema": "McpTrustError"
      },
      {
       "status": "404",
       "description": "Unknown id or cross-tenant probe (no existence leak).",
       "schema": "McpTrustError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/mcp-servers/{id}/pin",
     "operationId": "pinMcpServerToolset",
     "summary": "Pin the CURRENT cached toolset (rug-pull protection)",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Pin state.",
       "schema": "McpTrustState"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "McpTrustError"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier.",
       "schema": "McpTrustError"
      },
      {
       "status": "404",
       "description": "Unknown id or cross-tenant probe (no existence leak).",
       "schema": "McpTrustError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/mcp-servers/{id}/pin/accept",
     "operationId": "acceptMcpServerDrift",
     "summary": "Adopt the fresh toolset as cache + pin (accept detected drift)",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Updated trust state.",
       "schema": "McpTrustState"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "McpTrustError"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier.",
       "schema": "McpTrustError"
      },
      {
       "status": "404",
       "description": "Unknown id or cross-tenant probe (no existence leak).",
       "schema": "McpTrustError"
      }
     ]
    }
   ]
  },
  {
   "tag": "Memory",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/memory",
     "operationId": "listMemorySnippets",
     "summary": "List the caller's visible (non-expired) memory snippets",
     "description": "agent_id filters to exactly that agent's snippets; omitted lists the whole organization (org-level shared memory + all agents). Expired short-term snippets never surface. Results are newest first.",
     "permission": "agents.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "agent_id",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Restrict the listing to one agent's snippets."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Visible snippets of the caller's organization.",
       "schema": "MemSnippetsResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "MemErrorEnvelope"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/memory",
     "operationId": "putMemorySnippets",
     "summary": "Replace the snippet set of one (organization, agent) scope",
     "description": "Atomically replaces the memory of the caller's organization for the given agent_id (empty = org-level shared memory). expires_at is RFC3339 and drives short-term expiry; the optional embedding field accepts client-computed vectors, otherwise content is embedded with the service embedder. Embeddings are never echoed back.",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "MemPutMemoryRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The stored snippet set (post-normalization).",
       "schema": "MemSnippetsResponse"
      },
      {
       "status": "400",
       "description": "Malformed request body.",
       "schema": "MemErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "422",
       "description": "Validation failed (blank content, unknown scope, malformed expires_at).",
       "schema": "MemErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (e.g. store unavailable).",
       "schema": "MemErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "messaging",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/messaging/dlq",
     "operationId": "listMessagingDeadLetters",
     "summary": "List retained dead letters from agentos.dlq (OWNER/ADMIN/MEMBER/VIEWER)",
     "description": "",
     "permission": "runs.read (OWNER/ADMIN/MEMBER/VIEWER)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "limit",
       "in": "query",
       "required": false,
       "type": "integer",
       "description": "Maximum entries returned (<=0 or absent = unbounded)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The retained dead letters (oldest-first; empty array when the DLQ is drained).",
       "schema": "NcDLQEntry[]"
      },
      {
       "status": "409",
       "description": "Memory-effective messaging mode (no retained log; the messaging DLQ does not exist).",
       "schema": "NcErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "NcErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "NcErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/messaging/dlq/{seq}/requeue",
     "operationId": "requeueMessagingDeadLetter",
     "summary": "Requeue one dead letter back onto its original subject (OWNER/ADMIN)",
     "description": "",
     "permission": "queue.manage (OWNER/ADMIN; MEMBER/VIEWER denied 403)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "seq",
       "in": "path",
       "required": true,
       "type": "integer",
       "description": "The dead letter's 1-based sequence within the agentos.dlq retained log (stable across redeliveries; the requeue key)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The consumed dead-letter entry (it is no longer retained).",
       "schema": "NcDLQEntry"
      },
      {
       "status": "404",
       "description": "Unknown sequence (or already requeued \u2014 the entry no longer exists).",
       "schema": "NcErrorEnvelope"
      },
      {
       "status": "409",
       "description": "Memory-effective messaging mode (no retained log).",
       "schema": "NcErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "NcErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "NcErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/messaging/replay",
     "operationId": "replayMessagingSubject",
     "summary": "Replay a retained dispatch/coordination subject from a timestamp (OWNER/ADMIN)",
     "description": "",
     "permission": "queue.manage (OWNER/ADMIN; MEMBER/VIEWER denied 403)",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "NcReplayRequest",
     "bodyRequired": true,
     "bodyExample": "{\n  \"subject\": \"agentos.runs.create\",\n  \"from\": \"2026-09-10T12:00:00Z\"\n}",
     "responses": [
      {
       "status": "200",
       "description": "The replay report (replayed counts every re-published entry).",
       "schema": "NcReplayResult"
      },
      {
       "status": "400",
       "description": "The subject is not replayable (DLQ or outside the messaging plane).",
       "schema": "NcErrorEnvelope"
      },
      {
       "status": "409",
       "description": "Memory-effective messaging mode (no retained log).",
       "schema": "NcErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "NcErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "NcErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "notifications",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/notifications/subscriptions",
     "operationId": "listNotificationSubscriptions",
     "summary": "List the tenant's notification subscriptions",
     "description": "Returns all operational alert subscriptions of the authenticated organization, newest first.",
     "permission": "notifications.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Array of subscriptions (possibly empty).",
       "schema": "NsubSubscriptionList"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "NsubError"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "NsubError"
      },
      {
       "status": "500",
       "description": "Unexpected store failure.",
       "schema": "NsubError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/notifications/subscriptions",
     "operationId": "createNotificationSubscription",
     "summary": "Subscribe an existing webhook to operational event types",
     "description": "Creates a subscription routing the given event types to one existingnwebhook of the SAME organization. Validation:n * every `event_types` entry must be one of the pinned contract eventn types (run.failed, approval.requested, deployment.failed, \u2026) \u2014n unknown types return 400 and the message lists all valid types;n * at least one event type is required (explicit alert semantics;n use the webhooks API directly for wildcard sinks);n * `webhook_id` must resolve within the caller's organization \u2014n unknown or foreign-tenant ids return 404 (no existence leak).n Duplicate event types are de-duplicated silently.n",
     "permission": "notifications.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "NsubCreateSubscriptionRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Subscription created.",
       "schema": "NsubCreateSubscriptionResponse"
      },
      {
       "status": "400",
       "description": "Validation failure (malformed JSON body, unknown event type, empty  event type list, missing webhook_id). The message lists the valid  event types where applicable (error.code VALIDATION_ERROR or  INVALID_REQUEST).",
       "schema": "NsubError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "NsubError"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "NsubError"
      },
      {
       "status": "404",
       "description": "Target webhook does not exist within the caller's organization (error.code WEBHOOK_NOT_FOUND).",
       "schema": "NsubError"
      },
      {
       "status": "500",
       "description": "Unexpected store failure.",
       "schema": "NsubError"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/notifications/subscriptions/{id}",
     "operationId": "deleteNotificationSubscription",
     "summary": "Delete a notification subscription",
     "description": "Removes the subscription. Deleting an unknown or foreign-tenant id returns 404 (existence is not leaked across tenants). The bound webhook is untouched; deleting the webhook elsewhere cascades the subscription away (FK ON DELETE CASCADE).",
     "permission": "notifications.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Subscription id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deleted.",
       "schema": "NsubDeleteResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "NsubError"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "NsubError"
      },
      {
       "status": "404",
       "description": "Subscription not found within the caller's organization (error.code SUBSCRIPTION_NOT_FOUND).",
       "schema": "NsubError"
      }
     ]
    }
   ]
  },
  {
   "tag": "orchestration",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/orchestration/delegate",
     "operationId": "delegateOrchestrationTask",
     "summary": "Delegate a subtask from one agent to another (async)",
     "description": "",
     "permission": "orchestration.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "DelegateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "202",
       "description": "",
       "schema": "DelegateAccepted"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "OrchestrationError"
      },
      {
       "status": "403",
       "description": "",
       "schema": "OrchestrationError"
      },
      {
       "status": "409",
       "description": "",
       "schema": "DelegationFailedError"
      },
      {
       "status": "422",
       "description": "Validation error (missing/blank required fields, malformed JSON or limit, unknown message type, depth limit exceeded).",
       "schema": "OrchestrationError"
      },
      {
       "status": "500",
       "description": "",
       "schema": "DelegationFailedError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/orchestration/messages",
     "operationId": "sendOrchestrationMessage",
     "summary": "Send one point-to-point agent-to-agent message",
     "description": "",
     "permission": "orchestration.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "MessageSendRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "202",
       "description": "Delivered.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "OrchestrationError"
      },
      {
       "status": "403",
       "description": "",
       "schema": "OrchestrationError"
      },
      {
       "status": "404",
       "description": "",
       "schema": "OrchestrationError"
      },
      {
       "status": "409",
       "description": "INBOX_FULL \u2014 the target inbox is saturated; nothing was delivered.",
       "schema": "OrchestrationError"
      },
      {
       "status": "422",
       "description": "Validation error (missing/blank required fields, malformed JSON or limit, unknown message type, depth limit exceeded).",
       "schema": "OrchestrationError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/orchestration/agents/{id}/messages",
     "operationId": "listOrchestrationAgentMessages",
     "summary": "Read an agent's retained inbox (newest first)",
     "description": "",
     "permission": "orchestration.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "The agent id (org-scoped by the caller's claims)."
      },
      {
       "name": "limit",
       "in": "query",
       "required": false,
       "type": "integer",
       "description": "Maximum number of retained messages to return (0 / omitted = all)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The retained inbox (possibly empty).",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "OrchestrationError"
      },
      {
       "status": "403",
       "description": "",
       "schema": "OrchestrationError"
      },
      {
       "status": "404",
       "description": "",
       "schema": "OrchestrationError"
      },
      {
       "status": "422",
       "description": "Validation error (missing/blank required fields, malformed JSON or limit, unknown message type, depth limit exceeded).",
       "schema": "OrchestrationError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/orchestration/supervisor/run",
     "operationId": "runOrchestrationSupervisorTask",
     "summary": "Route one task through the supervisor to a worker agent",
     "description": "",
     "permission": "orchestration.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "SupervisorRunRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The delegated task completed.",
       "schema": "SupervisorRunResult"
      },
      {
       "status": "202",
       "description": "The supervised delegation was parked at the approval gate.",
       "schema": "SupervisorRunResult"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "OrchestrationError"
      },
      {
       "status": "403",
       "description": "",
       "schema": "OrchestrationError"
      },
      {
       "status": "409",
       "description": "",
       "schema": "OrchestrationError"
      },
      {
       "status": "422",
       "description": "Validation error (missing/blank required fields, malformed JSON or limit, unknown message type, depth limit exceeded).",
       "schema": "OrchestrationError"
      },
      {
       "status": "502",
       "description": "ALL_WORKERS_FAILED \u2014 every candidate attempt failed; attempts carries the trace.",
       "schema": "SupervisorFailedError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/orchestration/delegations",
     "operationId": "listOrchestrationDelegations",
     "summary": "Read the organization's delegation ledger (chain trace)",
     "description": "",
     "permission": "orchestration.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "run_id",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Filter to delegations whose parent or child run is this run id."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The delegation ledger (possibly empty).",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "OrchestrationError"
      },
      {
       "status": "403",
       "description": "",
       "schema": "OrchestrationError"
      }
     ]
    }
   ]
  },
  {
   "tag": "organization",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/organization",
     "operationId": "getCurrentOrganization",
     "summary": "Show the caller's current organization and the caller's platform role",
     "description": "Read operation (runs.read \u2014 all roles). Returns the tenant the caller authenticated into (derived from the auth claims, never from client input) plus the caller's role as embedded in the session/API key context. 404 ORGANIZATION_NOT_FOUND when the organization directory does not know the caller's tenant.",
     "permission": "runs.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The caller's organization and role.",
       "schema": "OrgInfoResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "404",
       "description": "The organization directory does not know the caller's tenant (error.code ORGANIZATION_NOT_FOUND).",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (details are never surfaced).",
       "schema": "OrgErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/organization/members",
     "operationId": "listOrganizationMembers",
     "summary": "List the members of the caller's organization (id, email, name, role, joined_at)",
     "description": "Read operation (runs.execute, MEMBER+ \u2014 viewers are excluded, same convention as the api-keys list). Returns the membership rows of the caller's tenant joined with the identity directory, ordered oldest member first. `name` is a display fallback derived from the email local part \u2014 the platform identity model has no display-name column (the SCIM 2.0 resource omits the attribute for the same reason), so no profile data is invented. `joined_at` is the membership row's creation timestamp. A membership row without a matching identity row projects empty email/name rather than being silently dropped.",
     "permission": "runs.execute",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Member list (possibly empty).",
       "schema": "OrgMemberListResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "OrgErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/organization/members",
     "operationId": "addOrganizationMember",
     "summary": "Add an existing platform user (by email) to the caller's organization",
     "description": "Write operation (users.manage, OWNER/ADMIN). Resolves the user by email through the identity directory \u2014 only EXISTING users can be invited (unknown emails are 404 USER_NOT_FOUND; the API never creates accounts or sends invitations). The membership is created in the CALLER's tenant with the requested role (case-insensitive, normalized). Duplicates are 409 ALREADY_MEMBER. Roles use the OWNER/ADMIN/MEMBER/VIEWER enum; anything else is 422 VALIDATION_ERROR. Adding a user as OWNER creates an additional owner (the last-owner guard then protects the pair). A removed member can be re-invited: re-adding reactivates their login through the same org-guarded store path SCIM uses for re-enabling. The tenant comes from the auth claims; the operation is audit-logged (organization.member_added).",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "OrgMemberAddRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Membership created; the member projection echoes the stored row (normalized role, real joined_at).",
       "schema": "OrgMemberMutationResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body (error.code INVALID_REQUEST).",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown email (error.code USER_NOT_FOUND) or the organization  directory does not know the caller's tenant (error.code  ORGANIZATION_NOT_FOUND).",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "409",
       "description": "The user already has a membership row in the caller's tenant (error.code ALREADY_MEMBER).",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "422",
       "description": "Blank email or role outside the OWNER/ADMIN/MEMBER/VIEWER enum (error.code VALIDATION_ERROR).",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "OrgErrorEnvelope"
      }
     ]
    },
    {
     "method": "PATCH",
     "path": "/organization/members/{userId}",
     "operationId": "updateOrganizationMemberRole",
     "summary": "Change a member's role (OWNER only; the last OWNER cannot be demoted)",
     "description": "Write operation (organization.manage \u2014 exactly OWNER, the same matrix as POST /scim/tokens: role changes are owner-level, which is why ADMINs \u2014 who can add and remove members \u2014 cannot change roles). The membership row is updated org-guarded: unknown and foreign-organization user ids are 404 MEMBER_NOT_FOUND with no existence leak. Demoting the last OWNER membership row of the tenant is rejected with 409 LAST_OWNER before anything is written. Re-asserting OWNER stays allowed. The operation is audit-logged (organization.member_role_updated, from_role/to_role metadata).",
     "permission": "organization.manage",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "userId",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "User id of the member (as returned by the member list)."
      }
     ],
     "bodySchema": "OrgMemberRoleUpdateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Role changed; the member projection echoes the stored row.",
       "schema": "OrgMemberMutationResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body (error.code INVALID_REQUEST).",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but not an OWNER (error.code FORBIDDEN).",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown or foreign-organization user id (error.code MEMBER_NOT_FOUND \u2014 no existence leak).",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "409",
       "description": "The target is the last OWNER membership row of the tenant (error.code LAST_OWNER).",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "422",
       "description": "Role outside the OWNER/ADMIN/MEMBER/VIEWER enum (error.code VALIDATION_ERROR).",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "OrgErrorEnvelope"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/organization/members/{userId}",
     "operationId": "removeOrganizationMember",
     "summary": "Remove a member (OWNER/ADMIN; the last OWNER cannot be removed; deactivates the member's login)",
     "description": "Write operation (users.manage, OWNER/ADMIN). Removes the membership row org-guarded (unknown/foreign ids are 404 MEMBER_NOT_FOUND) and deactivates the member's login through the SAME org-guarded store path the SCIM 2.0 service uses on deprovisioning \u2014 the member's next password login fails with auth.ErrAccountDisabled until they are re-invited. Removing the last OWNER membership row is rejected with 409 LAST_OWNER. For a cross-org invitee the org guard refuses the account deactivation (their login belongs to their home tenant) and only the membership row is removed. The operation is audit-logged (organization.member_removed, role-at-removal metadata).",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "userId",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "User id of the member (as returned by the member list)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Membership removed and login deactivated (see description).",
       "schema": "OrgMemberRemoveResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown or foreign-organization user id (error.code MEMBER_NOT_FOUND \u2014 no existence leak).",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "409",
       "description": "The target is the last OWNER membership row of the tenant (error.code LAST_OWNER).",
       "schema": "OrgErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "OrgErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "platform",
   "description": "Health, readiness and metrics.",
   "operations": [
    {
     "method": "GET",
     "path": "/",
     "operationId": "getServiceInfo",
     "summary": "Service identity banner (catch-all route)",
     "description": "Catch-all route returning the service identity. Reachable at the server root (outside the versioned base path). No authentication.",
     "permission": "",
     "auth": "public",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Service banner.",
       "schema": "object"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/healthz",
     "operationId": "getHealth",
     "summary": "Liveness probe",
     "description": "",
     "permission": "",
     "auth": "public",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Service is alive. Body is the literal string `ok`.",
       "schema": "string"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/metrics",
     "operationId": "getMetrics",
     "summary": "Application metrics snapshot",
     "description": "Returns in-process counters and the latest latency observations keyed by metric name, plus the current queue length. When the HTTP metrics middleware (internal/observability.MetricsMiddleware) is wired, entries follow the label format `http_requests_total{route=\"...\",method=\"...\",status=\"...\"}` and `http_request_duration_seconds{...}`.",
     "permission": "runs.read",
     "auth": "bearerToken or apiKey",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Metrics snapshot.",
       "schema": "MetricsResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/readyz",
     "operationId": "getReadiness",
     "summary": "Readiness probe",
     "description": "",
     "permission": "",
     "auth": "public",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Service is ready. Body is the literal string `ready`.",
       "schema": "string"
      }
     ]
    }
   ]
  },
  {
   "tag": "quality-gates",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/quality-gates/policies",
     "operationId": "createQualityGatePolicy",
     "summary": "Create quality-gate policy version 1",
     "description": "",
     "permission": "qualitygates.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "QualityGatePolicyInput",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Created (version 1).",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "QualityGatesError"
      },
      {
       "status": "403",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "422",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "500",
       "description": "Internal error.",
       "schema": "QualityGatesError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/quality-gates/policies",
     "operationId": "listQualityGatePolicies",
     "summary": "List the organization's quality-gate policies (latest versions)",
     "description": "",
     "permission": "qualitygates.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The latest version of every policy (name-sorted).",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "QualityGatesError"
      },
      {
       "status": "403",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "500",
       "description": "Internal error.",
       "schema": "QualityGatesError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/quality-gates/policies/{id}",
     "operationId": "getQualityGatePolicy",
     "summary": "Read one policy version",
     "description": "",
     "permission": "qualitygates.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Quality-gate policy id (UUID, as returned by the create operation)."
      },
      {
       "name": "version",
       "in": "query",
       "required": false,
       "type": "integer",
       "description": "Pin an exact policy version (positive integer; default latest)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The policy version.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "QualityGatesError"
      },
      {
       "status": "404",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "422",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "500",
       "description": "Internal error.",
       "schema": "QualityGatesError"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/quality-gates/policies/{id}",
     "operationId": "updateQualityGatePolicy",
     "summary": "Append a new immutable version of the policy",
     "description": "",
     "permission": "qualitygates.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Quality-gate policy id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": "QualityGatePolicyInput",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The new version.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "QualityGatesError"
      },
      {
       "status": "403",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "404",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "422",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "500",
       "description": "Internal error.",
       "schema": "QualityGatesError"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/quality-gates/policies/{id}",
     "operationId": "deleteQualityGatePolicy",
     "summary": "Delete every version of the policy",
     "description": "",
     "permission": "qualitygates.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Quality-gate policy id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deleted (id echoed back).",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "QualityGatesError"
      },
      {
       "status": "403",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "404",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "500",
       "description": "Internal error.",
       "schema": "QualityGatesError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/quality-gates/evaluate",
     "operationId": "evaluateQualityGate",
     "summary": "Evaluate a policy against explicit evidence refs",
     "description": "",
     "permission": "qualitygates.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "object",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The deterministic decision.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "QualityGatesError"
      },
      {
       "status": "404",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "422",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "500",
       "description": "Internal error.",
       "schema": "QualityGatesError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/quality-gates/exceptions",
     "operationId": "approveQualityGateException",
     "summary": "Approve a time-bounded exception on a policy",
     "description": "",
     "permission": "qualitygates.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "object",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The new policy version carrying the exception.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "QualityGatesError"
      },
      {
       "status": "403",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "404",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "422",
       "description": "",
       "schema": "QualityGatesError"
      },
      {
       "status": "500",
       "description": "Internal error.",
       "schema": "QualityGatesError"
      }
     ]
    }
   ]
  },
  {
   "tag": "queue",
   "description": "Worker task pull (dev pull model).",
   "operations": [
    {
     "method": "GET",
     "path": "/queue/pull",
     "operationId": "pullQueueTask",
     "summary": "Pull the next task (worker pull model)",
     "description": "Dequeues the next task (FIFO) for the calling worker. Returns `200` with the Task JSON, or `204 No Content` when the queue is empty. Intended for the worker's `AGENTOS_API_PULL=true` mode; a POST is accepted as an alias of GET. Requires `runs.execute`.",
     "permission": "runs.execute",
     "auth": "bearerToken or apiKey",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Next task.",
       "schema": "Task"
      },
      {
       "status": "204",
       "description": "Queue is empty.",
       "schema": null
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/queue/pull",
     "operationId": "pullQueueTaskPost",
     "summary": "Pull the next task (POST alias of GET)",
     "description": "",
     "permission": "runs.execute",
     "auth": "bearerToken or apiKey",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Next task (same schema as GET).",
       "schema": "Task"
      },
      {
       "status": "204",
       "description": "Queue is empty.",
       "schema": null
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/queue/tasks",
     "operationId": "listQueueTasks",
     "summary": "List the org's task-queue records (keyset-paginated, newest first)",
     "description": "Returns one page of the caller's organization's task records, newest enqueue first, with an opaque next_cursor for keyset continuation. Use `limit` to size the page (clamped server-side to [1, 200]; default 50) and pass `cursor` back unchanged to fetch the next page. `status` filters to exactly one engine state (queued | running | dead_letter | completed); an unknown value answers 400. Unscoped legacy tasks (predating org stamping) never match a scoped listing.",
     "permission": "runs.read (OWNER/ADMIN/MEMBER/VIEWER)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "status",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Exact engine-state filter. Omit to list every status."
      },
      {
       "name": "limit",
       "in": "query",
       "required": false,
       "type": "integer",
       "description": "Page size (clamped server-side; values <= 0 are a 400)."
      },
      {
       "name": "cursor",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Opaque keyset cursor from the previous response's next_cursor. Malformed cursors answer 400 INVALID_CURSOR."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "One keyset page of task records (newest enqueue first).",
       "schema": "QtTaskListResponse"
      },
      {
       "status": "400",
       "description": "Malformed cursor or invalid status/limit.",
       "schema": "QtErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": null
      }
     ]
    },
    {
     "method": "GET",
     "path": "/queue/tasks/{id}",
     "operationId": "getQueueTask",
     "summary": "Inspect one task (attempts, last error, timestamps)",
     "description": "Returns the caller's organization's record for one task, including the attempt counter, the requeues counter, the last handler error and the created/updated/requeued timestamps. Unknown AND foreign- organization ids answer the same 404 (no existence leak).",
     "permission": "runs.read (OWNER/ADMIN/MEMBER/VIEWER)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "The queue task identifier (as returned by the listing or by runs/create)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The task record.",
       "schema": "QtTask"
      },
      {
       "status": "404",
       "description": "Unknown or foreign-organization task id.",
       "schema": "QtErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": null
      }
     ]
    },
    {
     "method": "POST",
     "path": "/queue/tasks/{id}/requeue",
     "operationId": "requeueQueueTask",
     "summary": "Requeue a dead-lettered task (OWNER/ADMIN)",
     "description": "Revives a dead_letter task: attempts reset to 0, last_error cleared, the requeues counter incremented, requeued_at stamped and created_at preserved; the task is appended back to the work list so workers pick it up. Attempt bookkeeping restarts from zero (a fresh retry budget), while the requeues counter keeps the operator-visible history. Requeueing a task in any other state (queued, running, completed) is 409 CONFLICT \u2014 transient failures already requeue themselves inside the engine, so only the terminal dead_letter state is operator- requeueable. Unknown and foreign-organization ids are the same 404. Every successful requeue writes a queue.task_requeued audit row; refused operations write nothing. The operation is serialized per backend, so concurrent requeues yield exactly one work-list entry and one 409.",
     "permission": "queue.manage (OWNER/ADMIN; MEMBER/VIEWER denied 403)",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "The queue task identifier (as returned by the listing or by runs/create)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The requeued task record (status queued, attempts 0).",
       "schema": "QtTask"
      },
      {
       "status": "404",
       "description": "Unknown or foreign-organization task id.",
       "schema": "QtErrorEnvelope"
      },
      {
       "status": "409",
       "description": "The task is not in the requeueable dead_letter state.",
       "schema": "QtErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": null
      },
      {
       "status": "403",
       "description": "Caller lacks queue.manage (MEMBER/VIEWER). Body is the middleware's plain-text \"forbidden\".",
       "schema": null
      }
     ]
    },
    {
     "method": "GET",
     "path": "/queue/stats",
     "operationId": "getQueueStats",
     "summary": "Queue depth by status for the org",
     "description": "Returns the caller's organization's task depth for each of the four engine states (all keys always present, zero included) plus the total number of tracked tasks. Other organizations' tasks never count.",
     "permission": "runs.read (OWNER/ADMIN/MEMBER/VIEWER)",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Per-status depth and total.",
       "schema": "QtStatsResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": null
      }
     ]
    }
   ]
  },
  {
   "tag": "rate-limits",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/rate-limits",
     "operationId": "listRateLimits",
     "summary": "List the caller's organization's stored rate-limit rows (org defaults + subject rows)",
     "description": "",
     "permission": "limiter.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The org's rows (possibly empty \u2014 every scope unlimited).",
       "schema": "RateLimitListResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "RateLimitErrorEnvelope"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/rate-limits/{scope}",
     "operationId": "upsertRateLimitOrgDefault",
     "summary": "Upsert the scope's ORG DEFAULT limit row (subject_id null)",
     "description": "",
     "permission": "limiter.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "scope",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "One of org | agent | tool | model. The org scope has no subject."
      }
     ],
     "bodySchema": "RateLimitUpsertRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Row stored (read-back of the normalized, timestamped row).",
       "schema": "RateLimitUpsertResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body (error.code VALIDATION_ERROR).",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "422",
       "description": "",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "RateLimitErrorEnvelope"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/rate-limits/{scope}",
     "operationId": "deleteRateLimitOrgDefault",
     "summary": "Delete the scope's ORG DEFAULT limit row",
     "description": "",
     "permission": "limiter.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "scope",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "One of org | agent | tool | model. The org scope has no subject."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Row deleted.",
       "schema": "RateLimitDeleteResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "404",
       "description": "No row for (org, scope, subject) (error.code NOT_FOUND).",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "422",
       "description": "Unknown scope (error.code VALIDATION_ERROR).",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "RateLimitErrorEnvelope"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/rate-limits/{scope}/{subject_id}",
     "operationId": "upsertRateLimitSubject",
     "summary": "Upsert a subject-specific limit row (per agent / tool / model)",
     "description": "",
     "permission": "limiter.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "scope",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "One of org | agent | tool | model. The org scope has no subject."
      },
      {
       "name": "subject_id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Subject id \u2014 the agent id, tool id or model name the row applies to."
      }
     ],
     "bodySchema": "RateLimitUpsertRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Row stored (read-back of the normalized, timestamped row).",
       "schema": "RateLimitUpsertResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body (error.code VALIDATION_ERROR).",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "422",
       "description": "",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "RateLimitErrorEnvelope"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/rate-limits/{scope}/{subject_id}",
     "operationId": "deleteRateLimitSubject",
     "summary": "Delete a subject-specific limit row",
     "description": "",
     "permission": "limiter.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "scope",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "One of org | agent | tool | model. The org scope has no subject."
      },
      {
       "name": "subject_id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Subject id \u2014 the agent id, tool id or model name the row applies to."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Row deleted.",
       "schema": "RateLimitDeleteResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "404",
       "description": "No row for (org, scope, subject) (error.code NOT_FOUND).",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "422",
       "description": "Unknown scope (error.code VALIDATION_ERROR).",
       "schema": "RateLimitErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "RateLimitErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "residency",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/residency/policy",
     "operationId": "getResidencyPolicy",
     "summary": "Get the caller's organization residency policy",
     "description": "Returns the effective policy. An org without a stored policy is \"unpinned\" (default, 200).",
     "permission": "connectors.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The effective policy.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ResidencyError"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier (reads MEMBER+, writes/posture/check OWNER/ADMIN).",
       "schema": "ResidencyError"
      },
      {
       "status": "500",
       "description": "Internal error.",
       "schema": "ResidencyError"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/residency/policy",
     "operationId": "updateResidencyPolicy",
     "summary": "Pin / update the organization residency policy",
     "description": "",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "ResidencyPolicyInput",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Updated (mode is computed from the stored policy).",
       "schema": "object"
      },
      {
       "status": "400",
       "description": "Validation error (invalid region, empty strict policy, empty url).",
       "schema": "ResidencyError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ResidencyError"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier (reads MEMBER+, writes/posture/check OWNER/ADMIN).",
       "schema": "ResidencyError"
      },
      {
       "status": "500",
       "description": "Internal error.",
       "schema": "ResidencyError"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/residency/posture",
     "operationId": "getResidencyPosture",
     "summary": "Residency posture of the caller's organization",
     "description": "Compliance view \u2014 mode, pinning state and the last out-of-region evidence (flagged allow or strict deny).",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The posture.",
       "schema": "ResidencyPosture"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ResidencyError"
      },
      {
       "status": "403",
       "description": "Not permitted for this tier (reads MEMBER+, writes/posture/check OWNER/ADMIN).",
       "schema": "ResidencyError"
      },
      {
       "status": "500",
       "description": "Internal error.",
       "schema": "ResidencyError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/residency/check",
     "operationId": "checkResidency",
     "summary": "Explicit data-flow residency check for one URL",
     "description": "",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "object",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Allowed (in-region, flagged observe-mode, or unpinned).",
       "schema": "object"
      },
      {
       "status": "400",
       "description": "Validation error (invalid region, empty strict policy, empty url).",
       "schema": "ResidencyError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ResidencyError"
      },
      {
       "status": "403",
       "description": "",
       "schema": "ResidencyViolationError"
      },
      {
       "status": "500",
       "description": "Internal error.",
       "schema": "ResidencyError"
      }
     ]
    }
   ]
  },
  {
   "tag": "run-control",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/runs/{id}/cancel",
     "operationId": "cancelRun",
     "summary": "Cancel a run (idempotent, two-phase for in-flight runs)",
     "description": "",
     "permission": "runs.control",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Run id (UUID, as returned by the run-creation operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The updated run (status `cancelled`, or `cancelling` for an in-flight run awaiting its graceful unwind).",
       "schema": "RunLifecycleRunResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "RunLifecycleErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "RunLifecycleErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown or foreign run (no existence leak across tenants).",
       "schema": "RunLifecycleErrorEnvelope"
      },
      {
       "status": "409",
       "description": "Illegal transition (terminal run) or a lost optimistic race.",
       "schema": "RunLifecycleErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Internal error (or the runs service is unwired \u2192 503 RUNS_SERVICE_UNAVAILABLE).",
       "schema": "RunLifecycleErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/runs/{id}/pause",
     "operationId": "pauseRun",
     "summary": "Pause a run (idempotent)",
     "description": "",
     "permission": "runs.control",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Run id (UUID, as returned by the run-creation operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The updated run (status `paused`).",
       "schema": "RunLifecycleRunResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "RunLifecycleErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "RunLifecycleErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown or foreign run (no existence leak across tenants).",
       "schema": "RunLifecycleErrorEnvelope"
      },
      {
       "status": "409",
       "description": "Illegal transition (terminal or cancelling run) or a lost optimistic race.",
       "schema": "RunLifecycleErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Internal error (or the runs service is unwired \u2192 503 RUNS_SERVICE_UNAVAILABLE).",
       "schema": "RunLifecycleErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/runs/{id}/resume",
     "operationId": "resumeRun",
     "summary": "Resume a paused run (idempotent)",
     "description": "",
     "permission": "runs.control",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Run id (UUID, as returned by the run-creation operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The updated run (status `queued`).",
       "schema": "RunLifecycleRunResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "RunLifecycleErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "RunLifecycleErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown or foreign run (no existence leak across tenants).",
       "schema": "RunLifecycleErrorEnvelope"
      },
      {
       "status": "409",
       "description": "Illegal transition (not resumable) or a lost optimistic race.",
       "schema": "RunLifecycleErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Internal error (or the runs service is unwired \u2192 503 RUNS_SERVICE_UNAVAILABLE).",
       "schema": "RunLifecycleErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "runs",
   "description": "Agent executions and their event streams.",
   "operations": [
    {
     "method": "GET",
     "path": "/runs",
     "operationId": "listRuns",
     "summary": "List runs",
     "description": "Returns an envelope `{\"runs\":[...]}` scoped to the caller's organization (claims-derived tenant guard; cross-org ids are not visible). Unpaginated today.",
     "permission": "runs.read",
     "auth": "bearerToken or apiKey",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Runs envelope.",
       "schema": "ListRunsResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/runs",
     "operationId": "createRun",
     "summary": "Create and enqueue a run",
     "description": "Creates a run and enqueues an `agent.run` task. Responds 201 with the run ID and `status: \"queued\"`; actual execution happens in the worker. Governance enforcement (issue #75): when AGENTOS_POLICY_ENFORCEMENT is enabled (default ON), matching policies of the caller's organization are evaluated AFTER RBAC and BEFORE the quota gate and any run row is created. A deny decision answers 403 `policy_denied` with the decision reason and leaves no run behind (audit action `run.policy_denied`). An allow decision carrying `require_approval` parks the run in WAITING_APPROVAL, files an approval for the operators and answers 202 INSTEAD of enqueueing execution (audit action `run.approval_required`); approving the linked approval resumes and re-enqueues the run. An allowed run records the verdict on its timeline (step_type `policy`, see GET /runs/{id}/steps). A failing policy source answers 503 `policy_unavailable` (fail closed; audit action `run.policy_unavailable`). Organizations with no matching policy are unaffected (default allow). The optional `environment` request field scopes environment-conditional policies (default `production`) and is stamped onto the queue task so the worker's tool-level checks evaluate the same scope. The optional `estimated_cost_cents` feeds the `max_cost_cents` budget-guard condition (a deny rule with the cap matches over-budget requests).",
     "permission": "runs.execute",
     "auth": "bearerToken or apiKey",
     "params": [],
     "bodySchema": "CreateRunRequest",
     "bodyRequired": true,
     "bodyExample": "{\n  \"organization_id\": \"org-1\",\n  \"agent_id\": \"agent-1\",\n  \"input\": \"What is 2+2?\"\n}",
     "responses": [
      {
       "status": "201",
       "description": "Run created and task queued.",
       "schema": "CreateRunResponse"
      },
      {
       "status": "400",
       "description": "Invalid body or missing agent_id.",
       "schema": "Error"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "402",
       "description": "The organization's monthly run budget is exceeded and enforcement is  enabled (AGENTOS_BILLING_ENFORCEMENT). NO run row was created and NO  agent.run task was enqueued; the denial is audit-logged as  run.quota_denied. Upgrade the plan, wait for the period reset, or  disable enforcement.",
       "schema": "QuotaErrorEnvelope"
      },
      {
       "status": "403",
       "description": "A matching policy denied the action (issue #75). NO run row was  created and NO agent.run task was enqueued; the denial is audit-logged  as run.policy_denied with the matched policy id and reason. The error  message carries the machine decision reason, e.g.  'run denied by policy: deny by policy \"block production runs\"  (priority 10)'.",
       "schema": "PolEnfErrorEnvelope"
      },
      {
       "status": "503",
       "description": "Fail-closed misconfiguration or outage (issue #75): enforcement is  enabled but the policy decision source failed (503  policy_unavailable), or a require_approval policy matched while the  approvals service is not wired. Nothing executes and nothing is  enqueued; the failing-source case is audit-logged as  run.policy_unavailable.",
       "schema": "PolEnfErrorEnvelope"
      },
      {
       "status": "202",
       "description": "A winning allow policy carried require_approval (issue #75): the run  was created and parked in WAITING_APPROVAL, a pending approval was  filed for the operators (existing approvals flow) and NO execution  was enqueued (audit action run.approval_required). Approving the  linked approval resumes the run and re-enqueues its agent.run task;  rejecting it leaves the run parked.",
       "schema": "PolEnfWaitingApprovalResponse"
      },
      {
       "status": "405",
       "description": "Method not allowed (the endpoint is POST-only).",
       "schema": null
      }
     ]
    },
    {
     "method": "GET",
     "path": "/runs/{id}",
     "operationId": "getRun",
     "summary": "Get a run by ID",
     "description": "",
     "permission": "runs.read",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Run identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The run.",
       "schema": "Run"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/runs/{id}/events",
     "operationId": "streamRunEvents",
     "summary": "Stream run events (SSE) or fetch history (JSON)",
     "description": "Dual-protocol endpoint:n* `Accept: text/event-stream` \u2192 a live SSE stream. After the headers,n buffered history events are replayed first, then live events follow.n Each frame is `data: <RunEvent JSON>\\n\\n` (no `event:` field isn sent; clients should key off the `name` field inside the JSON,n e.g. `status.changed`). Comment keep-alive pings (`: ping`) may ben interleaved. The stream ends when the client disconnects.n* Any other `Accept` \u2192 JSON history envelopen `{\"run_id\": \"...\", \"events\": [RunEvent, ...]}`.n",
     "permission": "runs.read",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Run identifier (UUID v4 string)."
      },
      {
       "name": "Accept",
       "in": "header",
       "required": false,
       "type": "string",
       "description": "Set to `text/event-stream` to switch to the SSE protocol."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "SSE stream (text/event-stream) or JSON history depending on the  Accept header.",
       "schema": "RunEventsResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Run ID path segment missing or malformed.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/runs/{id}/events",
     "operationId": "publishRunEvent",
     "summary": "Publish an external event to a run's stream",
     "description": "Ingestion endpoint used by workers to report execution events. Accepts `{\"type\",\"name\",\"payload\"}` and fans the event out to SSE subscribers plus history. Returns 204 No Content. Note: today this route is registered under the `runs.read` permission gate; API keys authenticate with the OWNER role so worker callbacks succeed.",
     "permission": "runs.read",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Run identifier (UUID v4 string)."
      }
     ],
     "bodySchema": "PublishEventRequest",
     "bodyRequired": true,
     "bodyExample": "{\n  \"type\": \"status\",\n  \"name\": \"status.changed\",\n  \"payload\": {\n    \"status\": \"RUNNING\",\n    \"ts\": \"2025-01-01T12:00:01Z\"\n  }\n}",
     "responses": [
      {
       "status": "204",
       "description": "Event published.",
       "schema": null
      },
      {
       "status": "400",
       "description": "Invalid event body.",
       "schema": "Error"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/runs/{id}/steps",
     "operationId": "listRunSteps",
     "summary": "List the execution steps (trace) of a run",
     "description": "Returns the run's execution timeline. Issue #75 adds the step_type `policy`: one row per policy evaluation that shaped the run, with output_meta carrying `policy_id`, `decision`, `reason`, `require_approval` and the evaluation inputs (`action`, `agent_id`, `environment`, `estimated_cost_cents`). Tool steps blocked by policy keep their step_type `tool` with status `failed`, an error carrying the `policy_denied` (or `policy_unavailable`) code prefix, the denial observation as output, and the policy verdict in the step record.",
     "permission": "runs.read",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Run identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Step list (possibly empty) for the run.",
       "schema": "RunStepsResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      },
      {
       "status": "default",
       "description": "Unexpected server error.",
       "schema": "WfErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/runs/{id}/trace",
     "operationId": "getRunTrace",
     "summary": "Get the W3C trace id of a run's distributed execution trace",
     "description": "",
     "permission": "runs.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Run id (as returned by POST /runs)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The run's trace id (the run exists in the caller's org and a trace was recorded for it).",
       "schema": "TrRunTraceResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "TrErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the runs.read permission.",
       "schema": "TrErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Unknown run, foreign-org run, or no trace recorded for it in this process (error.code trace_not_available \u2014 identical shape for all three, no existence leak).",
       "schema": "TrErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "TrErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "Schedules",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/schedules",
     "operationId": "listSchedules",
     "summary": "List schedules of the caller's organization (oldest first)",
     "description": "Requires `schedules.read`. Unpaginated.",
     "permission": "schedules.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Schedules for the caller's tenant (possibly empty).",
       "schema": "ListSchedulesResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "500",
       "description": "Unexpected internal error.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/schedules/create",
     "operationId": "createSchedule",
     "summary": "Create a schedule (kind once | recurring | cron)",
     "description": "Requires `schedules.write`. Validation rules:n * `kind=once` requires `run_at` (RFC3339).n * `kind=recurring` requires `interval_seconds >= 60`.n * `kind=cron` requires a valid 5-field `cron_expr`n (minute hour day-of-month month day-of-week; supportsn `*`, numbers, ranges `a-b`, lists `a,b,c`, steps `*/n`)n and an IANA `timezone` (e.g. `UTC`, `Europe/Berlin`).n`next_run_at` is computed server-side; timezone defaults to `UTC` fornonce/recurring when omitted.n",
     "permission": "schedules.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "ScheduleCreateRequest",
     "bodyRequired": true,
     "bodyExample": "{\n  \"agent_id\": \"3f6c2b62-2b57-4f8e-9e4e-2f4d3b6c9a10\",\n  \"input\": \"daily digest\",\n  \"kind\": \"cron\",\n  \"cron_expr\": \"30 9 * * 1-5\",\n  \"timezone\": \"Europe/Berlin\"\n}",
     "responses": [
      {
       "status": "201",
       "description": "Schedule created (status `active`).",
       "schema": "ScheduleResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body.",
       "schema": "Error"
      },
      {
       "status": "422",
       "description": "Validation failure (kind rules, cron expression, timezone, run_at).",
       "schema": "Error"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/schedules/{id}",
     "operationId": "getSchedule",
     "summary": "Get one schedule",
     "description": "Requires `schedules.read`. Foreign-tenant ids surface as 404.",
     "permission": "schedules.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Schedule identifier (UUID)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The schedule.",
       "schema": "ScheduleResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Schedule not found (or belongs to another tenant).",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/schedules/{id}",
     "operationId": "deleteSchedule",
     "summary": "Delete a schedule",
     "description": "Requires `schedules.write`. Deleting an unknown or foreign id returns 404. The worker stops firing deleted schedules on its next poll.",
     "permission": "schedules.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deleted.",
       "schema": "DeleteScheduleResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Schedule not found (or belongs to another tenant).",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/schedules/{id}/pause",
     "operationId": "pauseSchedule",
     "summary": "Pause a schedule (active -> paused)",
     "description": "Requires `schedules.write`. Paused schedules never fire; `next_run_at` is preserved so resume continues the cadence (an overdue schedule fires once on the next worker poll).",
     "permission": "schedules.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Paused (returns the updated schedule).",
       "schema": "ScheduleResponse"
      },
      {
       "status": "409",
       "description": "Schedule is not active (paused/completed).",
       "schema": "Error"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Schedule not found (or belongs to another tenant).",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/schedules/{id}/resume",
     "operationId": "resumeSchedule",
     "summary": "Resume a paused schedule (paused -> active)",
     "description": "Requires `schedules.write`.",
     "permission": "schedules.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Resumed (returns the updated schedule).",
       "schema": "ScheduleResponse"
      },
      {
       "status": "409",
       "description": "Schedule is not paused (active/completed).",
       "schema": "Error"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Schedule not found (or belongs to another tenant).",
       "schema": "Error"
      }
     ]
    }
   ]
  },
  {
   "tag": "scim",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/scim/tokens",
     "operationId": "createScimToken",
     "summary": "Mint a SCIM bearer credential (plaintext shown exactly once)",
     "description": "OWNER-only (organization.manage). Creates one org-scoped SCIM bearer credential of the form `scim_<64 hex>`. Only the SHA-256 hex hash is persisted (UNIQUE, the api_keys.key_hash trust model); the plaintext is returned exactly once in `secret` and never stored or logged. The response metadata never echoes the hash.",
     "permission": "organization.manage",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Credential minted.",
       "schema": "ScimTokenCreated"
      },
      {
       "status": "401",
       "description": "",
       "schema": null
      },
      {
       "status": "403",
       "description": "Caller is not an OWNER (organization.manage).",
       "schema": "SsoError"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/scim/tokens/{id}",
     "operationId": "revokeScimToken",
     "summary": "Revoke one SCIM bearer credential (immediate; org-scoped)",
     "description": "Write operation (users.manage, OWNER/ADMIN). Revokes the SCIM bearer credential identified by `id` within the CALLER's organization \u2014 the tenant comes from the auth claims, never from the request. Success is 204 No Content; the revoked secret fails scim.Service.Authenticate (and therefore every /scim/v2/Users request) immediately. Unknown or foreign-organization ids surface as 404 SCIM_TOKEN_NOT_FOUND with no existence leak across tenants. The revocation is audit-logged as `scim_token.revoked` (resource scim/tokens/{id}; no secret or hash material).",
     "permission": "users.manage",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "SCIM token id (UUID, as returned by the mint operation createScimToken)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "204",
       "description": "Credential revoked (in the in-memory store also the idempotent  answer for an already-revoked id). No response body.",
       "schema": null
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ScimTokenRevokeErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission (users.manage =  exactly OWNER/ADMIN). No revocation happens and nothing is audited.",
       "schema": "ScimTokenRevokeErrorEnvelope"
      },
      {
       "status": "404",
       "description": "No live credential with this id within the caller's organization  (error.code SCIM_TOKEN_NOT_FOUND). Covers unknown ids and  cross-tenant ids without an existence leak; in the Postgres  store it also covers already-revoked ids (the org-guarded UPDATE  only matches revoked_at IS NULL rows).",
       "schema": "ScimTokenRevokeErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ScimTokenRevokeErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/scim/v2/Users",
     "operationId": "listScimUsers",
     "summary": "List (SCIM 2.0 ListResponse) with optional userName-eq filter",
     "description": "Returns the tenant's identities as a standard SCIM 2.0 ListResponse (urn:ietf:params:scim:api:messages:2.0:ListResponse) with totalResults/startIndex/itemsPerPage and an always-present `Resources` array (never null). The only supported filter is `userName eq \"<email>\"` (RFC 7644 \u00a73.4.2.2, case-insensitive, exact); any other filter is 400 and NEVER degrades into a full listing. Authentication is the dedicated scim_ bearer credential ONLY.",
     "permission": "",
     "auth": "scimBearer",
     "params": [
      {
       "name": "filter",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Supported form \u2014 userName eq \"user@example.com\""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The tenant's users (possibly an empty Resources array).",
       "schema": null
      },
      {
       "status": "400",
       "description": "Unsupported filter (ScimError, RFC 7644 \u00a73.12 envelope).",
       "schema": null
      },
      {
       "status": "401",
       "description": "Missing/unknown/revoked scim_ bearer credential (indistinguishable  on purpose).",
       "schema": null
      }
     ]
    },
    {
     "method": "POST",
     "path": "/scim/v2/Users",
     "operationId": "createScimUser",
     "summary": "JIT-provision one identity (SCIM 2.0 create)",
     "description": "Creates the identity from a SCIM 2.0 core-user document. userName is the login email (validated, lowercased) and `active` defaults to true. The user is created passwordless (invite-pending) and pinned to MEMBER \u2014 directory sync never mints privilege. Extra attributes (name, displayName, externalId, emails, password) are accepted and ignored by design. Duplicate emails conflict with 409 regardless of tenant (users.email is globally UNIQUE).",
     "permission": "",
     "auth": "scimBearer",
     "params": [],
     "bodySchema": null,
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Identity provisioned; Location carries the resource URL.",
       "schema": null
      },
      {
       "status": "400",
       "description": "Malformed body or invalid userName.",
       "schema": null
      },
      {
       "status": "401",
       "description": "Missing/unknown/revoked scim_ bearer credential.",
       "schema": null
      },
      {
       "status": "409",
       "description": "A user with this email already exists.",
       "schema": null
      }
     ]
    },
    {
     "method": "GET",
     "path": "/scim/v2/Users/{id}",
     "operationId": "getScimUser",
     "summary": "Read one identity (SCIM 2.0 core user)",
     "description": "Point read of one identity by SCIM id. Unknown AND foreign-tenant ids both answer 404 without an existence leak.",
     "permission": "",
     "auth": "scimBearer",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "SCIM resource id (the platform user id)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The identity.",
       "schema": null
      },
      {
       "status": "401",
       "description": "Missing/unknown/revoked scim_ bearer credential.",
       "schema": null
      },
      {
       "status": "404",
       "description": "Unknown or foreign id (no existence leak).",
       "schema": null
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/scim/v2/Users/{id}",
     "operationId": "replaceScimUser",
     "summary": "Replace an identity (userName immutable)",
     "description": "SCIM 2.0 full replace of the mutable attributes. userName is immutable in this deployment (it IS the login credential): a mismatching userName is 400, so a directory rename can never hijack an account. Full-replace semantics per RFC 7644 \u00a73.5.1: an omitted `active` resets to the default true.",
     "permission": "",
     "auth": "scimBearer",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "SCIM resource id (the platform user id)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The replaced identity.",
       "schema": null
      },
      {
       "status": "400",
       "description": "Malformed body, invalid userName or userName mismatch (immutable).",
       "schema": null
      },
      {
       "status": "401",
       "description": "Missing/unknown/revoked scim_ bearer credential.",
       "schema": null
      },
      {
       "status": "404",
       "description": "Unknown or foreign id.",
       "schema": null
      }
     ]
    },
    {
     "method": "PATCH",
     "path": "/scim/v2/Users/{id}",
     "operationId": "patchScimUser",
     "summary": "Patch an identity (replace active \u2014 deprovisioning)",
     "description": "SCIM 2.0 PATCH. Supports the standard deprovisioning operations: `{\"op\":\"replace\",\"path\":\"active\",\"value\":false}` and the pathless `{\"op\":\"replace\",\"value\":{\"active\":false}}` (op case-insensitive, boolean values may be string-encoded for stock IdP connectors). Anything else is 400 and applied nothing. Disabling the user blocks password login platform-wide; re-enabling restores it.",
     "permission": "",
     "auth": "scimBearer",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "SCIM resource id (the platform user id)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The patched identity.",
       "schema": null
      },
      {
       "status": "400",
       "description": "Unsupported or malformed patch operations (nothing applied).",
       "schema": null
      },
      {
       "status": "401",
       "description": "Missing/unknown/revoked scim_ bearer credential.",
       "schema": null
      },
      {
       "status": "404",
       "description": "Unknown or foreign id.",
       "schema": null
      }
     ]
    }
   ]
  },
  {
   "tag": "secrets",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/secrets",
     "operationId": "createSecret",
     "summary": "Create one org-scoped secret (value sealed server-side)",
     "description": "Write operation (agents.write, OWNER/ADMIN). Body carries the secret `name` and plaintext `value`; the value is sealed with AES-256-GCM under the current master-key version before storage and is never echoed back, logged, or persisted in plaintext. Names must match `[A-Za-z0-9][A-Za-z0-9._-]{0,254}` (URL-path safe, env-var friendly); values are capped at 64 KiB. Duplicate (org, name) among live secrets is a 409. The tenant is derived from the auth claims \u2014 any client-supplied organization id is ignored.",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "SecCreateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Secret created (metadata only \u2014 the value is never echoed).",
       "schema": "SecResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body (error.code INVALID_REQUEST).",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "409",
       "description": "A live secret with the same name already exists in the caller's organization (error.code SECRET_ALREADY_EXISTS).",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "422",
       "description": "Validation error: missing org/name/value, invalid name charset, or  value above the 64 KiB limit (error.code VALIDATION_ERROR).",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (also covers key-material failures; details are never surfaced).",
       "schema": "SecErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/secrets",
     "operationId": "listSecrets",
     "summary": "List secret metadata for the caller's organization (never values)",
     "description": "Read operation (runs.execute, MEMBER+ \u2014 viewers are excluded because even secret NAMES are sensitive). Returns metadata only: name, the key_version that encrypted each value, created_by and timestamps. The response schema has no value field and the server never includes the plaintext in list responses. Results are ordered by name ASC.",
     "permission": "runs.execute",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Metadata list (possibly empty; values are structurally absent).",
       "schema": "SecListResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "SecErrorEnvelope"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/secrets/{name}",
     "operationId": "deleteSecret",
     "summary": "Soft-delete one secret (tombstone; name becomes reusable)",
     "description": "Write operation (agents.write, OWNER/ADMIN). Sets the deleted_at tombstone; every subsequent read/resolve behaves as 404 and the name can be recreated fresh. Foreign or unknown names surface as 404 (no existence leak across tenants). Deletion is audit-logged.",
     "permission": "agents.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "name",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Secret name (unique per organization among live secrets). Must match  `[A-Za-z0-9][A-Za-z0-9._-]{0,254}`.  "
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Secret tombstoned.",
       "schema": "SecDeleteResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission for this operation.",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Secret not found within the caller's organization (error.code SECRET_NOT_FOUND).",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "SecErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/secrets/{name}/reveal",
     "operationId": "revealSecret",
     "summary": "Return the plaintext value once (OWNER only, audit-logged)",
     "description": "OWNER-only operation (organization.manage). Decrypts the sealed value and returns it exactly once inside the secret object; this is the only response in the whole API that carries a plaintext secret. Every call writes a `secret.revealed` audit entry (the value itself is never part of the audit metadata). Foreign/unknown/tombstoned names are 404.",
     "permission": "organization.manage",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "name",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Secret name (unique per organization among live secrets). Must match  `[A-Za-z0-9][A-Za-z0-9._-]{0,254}`.  "
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The decrypted value (single-use response; never cached or logged).",
       "schema": "SecRevealResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but not an OWNER (organization.manage is OWNER-only).",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Secret not found within the caller's organization (error.code SECRET_NOT_FOUND).",
       "schema": "SecErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (e.g. decryption failed under the configured master key; details are never surfaced).",
       "schema": "SecErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "showback",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/showback",
     "operationId": "getShowbackReport",
     "summary": "Per-agent usage/cost rollup for a period",
     "description": "",
     "permission": "",
     "auth": "bearerAuth",
     "params": [
      {
       "name": "from",
       "in": "query",
       "required": true,
       "type": "string",
       "description": ""
      },
      {
       "name": "to",
       "in": "query",
       "required": true,
       "type": "string",
       "description": ""
      },
      {
       "name": "group_by",
       "in": "query",
       "required": false,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Report.",
       "schema": "ShowbackReport"
      },
      {
       "status": "400",
       "description": "Invalid range or group_by.",
       "schema": "ShowbackError"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "ShowbackError"
      },
      {
       "status": "403",
       "description": "Not permitted (billing read tier).",
       "schema": "ShowbackError"
      }
     ]
    }
   ]
  },
  {
   "tag": "synth",
   "description": "",
   "operations": [
    {
     "method": "POST",
     "path": "/synth/projects",
     "operationId": "createSynthProject",
     "summary": "Create a project container for synthetic testing",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "SynthProjectCreateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Project created.",
       "schema": "SynthProjectResponse"
      },
      {
       "status": "400",
       "description": "Invalid input (400 VALIDATION_ERROR).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/synth/projects",
     "operationId": "listSynthProjects",
     "summary": "List the organization's projects",
     "description": "",
     "permission": "synth.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Projects ordered by creation time (empty list, never null).",
       "schema": "SynthProjectListResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/synth/projects/{id}",
     "operationId": "getSynthProject",
     "summary": "Get one project (foreign-org ids are 404)",
     "description": "",
     "permission": "synth.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The project.",
       "schema": "SynthProjectResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/synth/projects/{id}",
     "operationId": "updateSynthProject",
     "summary": "Rename a project",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": "SynthProjectCreateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The renamed project.",
       "schema": "SynthProjectResponse"
      },
      {
       "status": "400",
       "description": "Invalid input (400 VALIDATION_ERROR).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/synth/projects/{id}",
     "operationId": "deleteSynthProject",
     "summary": "Delete a project (cascades to its targets and environments)",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deleted.",
       "schema": "SynthDeletedResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/synth/targets",
     "operationId": "createSynthTarget",
     "summary": "",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "SynthTargetCreateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Target created (verified=false).",
       "schema": "SynthTargetResponse"
      },
      {
       "status": "400",
       "description": "Invalid input (400 VALIDATION_ERROR).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/synth/targets",
     "operationId": "listSynthTargets",
     "summary": "List targets, optionally narrowed to one project",
     "description": "",
     "permission": "synth.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "project_id",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "When present, only targets of this project are returned."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Targets ordered by creation time (empty list, never null).",
       "schema": "SynthTargetListResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/synth/targets/{id}",
     "operationId": "getSynthTarget",
     "summary": "Get one target (never carries challenge-token material)",
     "description": "",
     "permission": "synth.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The target.",
       "schema": "SynthTargetResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/synth/targets/{id}",
     "operationId": "updateSynthTarget",
     "summary": "Rename a target (verification state is owned by the verify flow)",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": "SynthTargetUpdateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The renamed target.",
       "schema": "SynthTargetResponse"
      },
      {
       "status": "400",
       "description": "Invalid input (400 VALIDATION_ERROR).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/synth/targets/{id}",
     "operationId": "deleteSynthTarget",
     "summary": "Delete a target (dependent environments are removed with it)",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deleted.",
       "schema": "SynthDeletedResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/synth/targets/{id}/verify",
     "operationId": "verifySynthTarget",
     "summary": "Issue the ownership challenge OR confirm it (verified=true)",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": "SynthVerifyRequest",
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "",
       "schema": "SynthVerifyResponse"
      },
      {
       "status": "400",
       "description": "Invalid input (400 VALIDATION_ERROR).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Challenge token mismatch (VERIFICATION_FAILED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/synth/targets/{id}/environments",
     "operationId": "createSynthEnvironment",
     "summary": "Bind a runnable context with hard safety rails to a target",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": "SynthEnvironmentSpec",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Environment created (kind-derived default mode applied).",
       "schema": "SynthEnvironmentResponse"
      },
      {
       "status": "400",
       "description": "Invalid input (400 VALIDATION_ERROR).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/synth/targets/{id}/environments",
     "operationId": "listSynthTargetEnvironments",
     "summary": "List a target's environments",
     "description": "",
     "permission": "synth.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Environments of the target (empty list, never null).",
       "schema": "SynthEnvironmentListResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/synth/environments/{id}",
     "operationId": "getSynthEnvironment",
     "summary": "Get one environment",
     "description": "",
     "permission": "synth.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The environment.",
       "schema": "SynthEnvironmentResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/synth/environments/{id}",
     "operationId": "updateSynthEnvironment",
     "summary": "Replace the safety posture (re-validated; production cannot escalate)",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": "SynthEnvironmentSpec",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The updated environment.",
       "schema": "SynthEnvironmentResponse"
      },
      {
       "status": "400",
       "description": "Invalid input (400 VALIDATION_ERROR).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/synth/environments/{id}",
     "operationId": "deleteSynthEnvironment",
     "summary": "Delete an environment",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deleted.",
       "schema": "SynthDeletedResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/synth/personas",
     "operationId": "createSynthPersona",
     "summary": "Register a synthetic identity profile",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "SynthPersonaSpec",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Persona created (version 1).",
       "schema": "SynthPersonaResponse"
      },
      {
       "status": "400",
       "description": "Invalid input (400 VALIDATION_ERROR).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "403",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/synth/personas",
     "operationId": "listSynthPersonas",
     "summary": "List the organization's personas",
     "description": "",
     "permission": "synth.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Personas ordered by creation time (empty list, never null).",
       "schema": "SynthPersonaListResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/synth/personas/{id}",
     "operationId": "getSynthPersona",
     "summary": "Get one persona",
     "description": "",
     "permission": "synth.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The persona.",
       "schema": "SynthPersonaResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "PUT",
     "path": "/synth/personas/{id}",
     "operationId": "updateSynthPersona",
     "summary": "Update a persona (bumps the version journeys pin against)",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": "SynthPersonaSpec",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The updated persona with an incremented version.",
       "schema": "SynthPersonaResponse"
      },
      {
       "status": "400",
       "description": "Invalid input (400 VALIDATION_ERROR).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/synth/personas/{id}",
     "operationId": "deleteSynthPersona",
     "summary": "Delete a persona",
     "description": "",
     "permission": "synth.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Resource id (UUID, as returned by the create operation)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deleted.",
       "schema": "SynthDeletedResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (401 UNAUTHORIZED).",
       "schema": "SynthErrorEnvelope"
      },
      {
       "status": "404",
       "description": "",
       "schema": "SynthErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "Tools",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/tools",
     "operationId": "listRegisteredTools",
     "summary": "List the runtime's registered tools (name, description, input schema)",
     "description": "",
     "permission": "agents.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The registered tools, sorted by name; each entry carries the tool's input schema when it publishes one.",
       "schema": "ToolListResponse"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials (see main spec auth responses).",
       "schema": null
      },
      {
       "status": "403",
       "description": "Authenticated but not permitted (agents.read).",
       "schema": null
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "ToolCatalogErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "Usage",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/usage/costs",
     "operationId": "getUsageCosts",
     "summary": "Tenant usage-cost report aggregated from run costs",
     "description": "Aggregates `runs.cost_cents` for the caller's organization over the half-open [from, to) window. `from`/`to` accept RFC3339 timestamps or bare `YYYY-MM-DD` UTC dates; when omitted the window defaults to the 30 days ending now (capped at 366 days). `group_by` selects the aggregation dimension: `day` (UTC day buckets), `agent` (runs grouped by the run's agent_id) or `model` (grouped by the serving model, resolved from the agents catalog at query time). Costs are USD cents; runs without pricing data count as 0 and are still included in the `runs` counters.",
     "permission": "usage.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "from",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Window start (inclusive). RFC3339 or YYYY-MM-DD (UTC). Defaults to `to` minus 30 days."
      },
      {
       "name": "to",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Window end (exclusive). RFC3339 or YYYY-MM-DD (UTC). Defaults to now."
      },
      {
       "name": "group_by",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Aggregation dimension (default `day`)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Aggregated cost report for the caller's organization.",
       "schema": "UsgUsageCostsResponse"
      },
      {
       "status": "400",
       "description": "Invalid query: unknown `group_by` (machine code  `INVALID_GROUP_BY`) or an unparsable/inverted/too-wide window  (machine code `INVALID_TIME_RANGE`).",
       "schema": "UsgErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "UsgErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the `usage.read` permission.",
       "schema": "UsgErrorEnvelope"
      },
      {
       "status": "405",
       "description": "Method not allowed (only GET is served).",
       "schema": "string"
      },
      {
       "status": "503",
       "description": "Runs service not wired into the API server (machine code `USAGE_UNAVAILABLE`).",
       "schema": "UsgErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "usage",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/usage/meters",
     "operationId": "listUsageMeters",
     "summary": "Aggregated usage meters for the caller's organization",
     "description": "Read (runs.execute, MEMBER+). Aggregates the caller's organization's metered activity over the half-open [from, to) window: runs_count (metered runs \u2014 the same runs.cost_cents aggregation that powers GET /usage/costs; only the counts are taken) and tool_calls_count (executed tool steps, step_type='tool'). sandbox_seconds is intentionally absent: no durable sandbox-duration record exists today, and data is never invented. When STRIPE_API_KEY is configured, a successful read additionally triggers an async, idempotent sync of these meters to Stripe's /v1/usage_records (see the fragment description); without the env key nothing leaves the process.",
     "permission": "runs.execute",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "from",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Window start (RFC3339 timestamp or YYYY-MM-DD date). Defaults to `to` minus 30 days."
      },
      {
       "name": "to",
       "in": "query",
       "required": false,
       "type": "string",
       "description": "Window end, exclusive (RFC3339 timestamp or YYYY-MM-DD date). Defaults to now."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The aggregated meters with the effective window echo.",
       "schema": "SmMetersResponse"
      },
      {
       "status": "400",
       "description": "Unparsable or inverted window, or a window exceeding 366 days  (error.code INVALID_TIME_RANGE).",
       "schema": "SmErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing or invalid credentials.",
       "schema": "SmErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but the role lacks the required permission (meters need  MEMBER+ via runs.execute; margin is OWNER-only via  organization.manage).",
       "schema": "SmErrorEnvelope"
      },
      {
       "status": "405",
       "description": "Method not allowed (the endpoint is GET-only).",
       "schema": null
      },
      {
       "status": "500",
       "description": "Unexpected server failure.",
       "schema": "SmErrorEnvelope"
      },
      {
       "status": "503",
       "description": "The meter source is not wired in this deployment  (error.code METER_SOURCE_UNAVAILABLE).",
       "schema": "SmErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "webhooks",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/webhooks",
     "operationId": "listWebhooks",
     "summary": "List the tenant's outbound webhook endpoints",
     "description": "Returns all webhook registrations of the authenticated organization, newest first. The signing secret is never included in list responses \u2014 only `secret_set` (always `true` for records that carry a secret hash).",
     "permission": "webhooks.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Array of webhooks (possibly empty).",
       "schema": "WebhookList"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "500",
       "description": "Unexpected store failure.",
       "schema": "WebhookError"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/webhooks/create",
     "operationId": "createWebhook",
     "summary": "Register an outbound webhook endpoint",
     "description": "Creates a webhook subscribed to the given event types (empty `events` array = subscribe to ALL event types). An HMAC signing secret is generated and returned in this response body as `secret` \u2014 EXACTLY ONCE; only its SHA-256 hash is persisted. Invalid input (non-http(s) URL, unknown event type) returns 422.",
     "permission": "webhooks.write",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "CreateWebhookRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "201",
       "description": "Webhook created; the one-time signing secret is included.",
       "schema": "CreateWebhookResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body.",
       "schema": "WebhookError"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "422",
       "description": "Validation failure (invalid URL or unknown event type).",
       "schema": "WebhookError"
      }
     ]
    },
    {
     "method": "DELETE",
     "path": "/webhooks/{id}",
     "operationId": "deleteWebhook",
     "summary": "Delete a webhook endpoint",
     "description": "Removes the webhook (and its delivery records, via FK cascade). Deleting an unknown or foreign-tenant id returns 404 (existence is not leaked across tenants).",
     "permission": "webhooks.write",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Deleted.",
       "schema": "WebhookDeleteResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/webhooks/{id}/deliveries",
     "operationId": "listWebhookDeliveries",
     "summary": "List recent delivery attempts of one webhook",
     "description": "Returns the webhook's delivery records, newest first. Each record mirrors the LATEST attempt (attempts / last_status_code / latency_ms / error are updated in place by the delivery worker after every retry). `limit` defaults to 50 and is capped at 500; non-integer or non-positive values return 400.",
     "permission": "webhooks.read",
     "auth": "apiKey or bearerToken",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": ""
      },
      {
       "name": "limit",
       "in": "query",
       "required": false,
       "type": "integer",
       "description": ""
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Array of delivery records (possibly empty).",
       "schema": "WebhookDeliveryList"
      },
      {
       "status": "400",
       "description": "Invalid limit parameter.",
       "schema": "WebhookError"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials (plain text body today; JSON model is the target).",
       "schema": "Error"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission or organization access.",
       "schema": "Error"
      },
      {
       "status": "404",
       "description": "Resource not found.",
       "schema": "Error"
      }
     ]
    }
   ]
  },
  {
   "tag": "workflow-runs",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/workflow-runs/{id}",
     "operationId": "getWorkflowRun",
     "summary": "Get one workflow run with its node runs",
     "description": "",
     "permission": "workflows.read",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Workflow run identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Workflow run with the per-node run mapping.",
       "schema": "WorkflowRunResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Resource not found (or owned by another tenant \u2014 cross-tenant access surfaces as 404, never 403).",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server error.",
       "schema": "WfErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/workflow-runs/{id}/nodes",
     "operationId": "listWorkflowRunNodes",
     "summary": "Get the checkpointed node timeline of a workflow run",
     "description": "Returns every node execution checkpoint of one workflow run, oldest first: one entry per (node, attempt) pair. Entries are the durable checkpoints written synchronously by the executor/worker, so the response reflects the exact state the recovery pass acts on. `status` uses the workflow status vocabulary (pending, running, waiting_approval, completed, failed, cancelled, timeout \u2014 `timeout` is assigned by the recovery watchdog). `error_code` is a machine code (NODE_ORPHANED, WORKFLOW_RUN_TIMEOUT, ...) or `null` while healthy. A run owned by another tenant surfaces as 404, never 403.",
     "permission": "workflows.read",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Workflow run identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The checkpointed node timeline (may be empty while the DAG expansion has not recorded any node yet).",
       "schema": "Wf3NodeTimelineResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials.",
       "schema": "Wf3ErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission.",
       "schema": "Wf3ErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Workflow run not found (or owned by another tenant \u2014 cross-tenant access surfaces as 404, never 403).",
       "schema": "Wf3ErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server error.",
       "schema": "Wf3ErrorEnvelope"
      }
     ]
    }
   ]
  },
  {
   "tag": "workflows",
   "description": "",
   "operations": [
    {
     "method": "GET",
     "path": "/workflows",
     "operationId": "listWorkflows",
     "summary": "List the tenant's workflows",
     "description": "",
     "permission": "workflows.read",
     "auth": "bearerToken or apiKey",
     "params": [],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Workflows (summary shape, newest first).",
       "schema": "WorkflowListResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server error.",
       "schema": "WfErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/workflows/create",
     "operationId": "createWorkflow",
     "summary": "Create a draft workflow from a DSL",
     "description": "Runs DSL validation first: invalid definitions are rejected with 422 and the structured error list (code, message, node_id). The workflow starts as `draft` with `current_version` 0. The tenant is taken from the authenticated principal; client-supplied organization ids are ignored.",
     "permission": "workflows.write",
     "auth": "bearerToken or apiKey",
     "params": [],
     "bodySchema": "CreateWorkflowRequest",
     "bodyRequired": true,
     "bodyExample": "{\n  \"name\": \"Deploy Flow\",\n  \"description\": \"Plan then deploy with a sign-off gate\",\n  \"dsl\": {\n    \"nodes\": [\n      {\n        \"id\": \"n1\",\n        \"type\": \"agent\",\n        \"name\": \"Planner\",\n        \"config\": {\n          \"agent_id\": \"agent-1\",\n          \"input\": \"{{input}}\"\n        }\n      },\n      {\n        \"id\": \"n2\",\n        \"type\": \"approval\",\n        \"name\": \"Gate\",\n        \"config\": {\n          \"action\": \"deploy\",\n          \"risk\": \"high\"\n        }\n      }\n    ],\n    \"edges\": [\n      {\n        \"from\": \"n1\",\n        \"to\": \"n2\",\n        \"condition\": \"on_success\"\n      }\n    ]\n  }\n}",
     "responses": [
      {
       "status": "201",
       "description": "Workflow created (draft).",
       "schema": "WorkflowDetailResponse"
      },
      {
       "status": "400",
       "description": "Malformed JSON body.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "422",
       "description": "DSL validation failed. Body: `{\"errors\":[{\"code\",\"message\",\"node_id\"}]}`.  Error codes: empty_graph, missing_node_id, duplicate_node_id,  unknown_node_type, missing_config, missing_node_ref,  invalid_edge_condition, cycle_detected.",
       "schema": "WorkflowValidationErrors"
      },
      {
       "status": "500",
       "description": "Unexpected server error.",
       "schema": "WfErrorEnvelope"
      }
     ]
    },
    {
     "method": "GET",
     "path": "/workflows/{id}",
     "operationId": "getWorkflow",
     "summary": "Get one workflow with its published versions",
     "description": "",
     "permission": "workflows.read",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Workflow identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Workflow detail including immutable versions.",
       "schema": "WorkflowDetailWithVersionsResponse"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Resource not found (or owned by another tenant \u2014 cross-tenant access surfaces as 404, never 403).",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "500",
       "description": "Unexpected server error.",
       "schema": "WfErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/workflows/{id}/execute",
     "operationId": "executeWorkflow",
     "summary": "Execute the workflow (expand its DAG into agent runs)",
     "description": "Expands the DAG in topological order: one agent run per agent/tool node (tool nodes without an explicit config.agent_id ride the first agent node), every run is enqueued on the worker queue. An approval node creates a pending approval, pauses the run preceding it and flips the workflow run to `waiting_approval`; approving the approval resumes the paused run. Structural nodes (condition/parallel/delay/webhook/end) record completed node runs without agent runs.",
     "permission": "workflows.execute",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Workflow identifier (UUID v4 string)."
      }
     ],
     "bodySchema": "ExecuteWorkflowRequest",
     "bodyRequired": true,
     "bodyExample": "{\n  \"input\": \"Deploy the payments service\"\n}",
     "responses": [
      {
       "status": "200",
       "description": "DAG expanded; node runs created and tasks enqueued.",
       "schema": "ExecutionResult"
      },
      {
       "status": "400",
       "description": "Malformed JSON body.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Resource not found (or owned by another tenant \u2014 cross-tenant access surfaces as 404, never 403).",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "422",
       "description": "DSL validation failures.",
       "schema": "WorkflowValidationErrors"
      },
      {
       "status": "503",
       "description": "The execution engine (runs/queue services) is not wired.",
       "schema": "WfErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/workflows/{id}/publish",
     "operationId": "publishWorkflow",
     "summary": "Publish the current DSL as the next immutable version",
     "description": "Snapshots the workflow's DSL into a new version (1, 2, \u2026) and marks the workflow published. Invalid DSLs are rejected with 422.",
     "permission": "workflows.write",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Workflow identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "Version published.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Resource not found (or owned by another tenant \u2014 cross-tenant access surfaces as 404, never 403).",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "422",
       "description": "DSL validation failures (publish requires a valid DSL).",
       "schema": "WorkflowValidationErrors"
      },
      {
       "status": "500",
       "description": "Unexpected server error.",
       "schema": "WfErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/workflows/{id}/validate",
     "operationId": "validateWorkflow",
     "summary": "Validate the workflow's current DSL",
     "description": "",
     "permission": "workflows.read",
     "auth": "bearerToken or apiKey",
     "params": [
      {
       "name": "id",
       "in": "path",
       "required": true,
       "type": "string",
       "description": "Workflow identifier (UUID v4 string)."
      }
     ],
     "bodySchema": null,
     "bodyRequired": false,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The DSL is valid.",
       "schema": "object"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "404",
       "description": "Resource not found (or owned by another tenant \u2014 cross-tenant access surfaces as 404, never 403).",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "422",
       "description": "Validation failures (same body as create).",
       "schema": "WorkflowValidationErrors"
      },
      {
       "status": "500",
       "description": "Unexpected server error.",
       "schema": "WfErrorEnvelope"
      }
     ]
    },
    {
     "method": "POST",
     "path": "/workflows/validate",
     "operationId": "validateWorkflowDSL",
     "summary": "Validate an ad-hoc workflow DSL document (stateless, nothing persisted)",
     "description": "",
     "permission": "workflows.read",
     "auth": "apiKey or bearerToken",
     "params": [],
     "bodySchema": "WfValidateRequest",
     "bodyRequired": true,
     "bodyExample": null,
     "responses": [
      {
       "status": "200",
       "description": "The DSL document passes every validation rule.",
       "schema": "WfValidateSuccess"
      },
      {
       "status": "400",
       "description": "Malformed JSON body (error.code INVALID_REQUEST).",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "401",
       "description": "Missing/invalid credentials.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "403",
       "description": "Authenticated but lacking the required permission.",
       "schema": "WfErrorEnvelope"
      },
      {
       "status": "422",
       "description": "",
       "schema": "WfValidateFailure"
      },
      {
       "status": "500",
       "description": "Unexpected server failure (details are never surfaced).",
       "schema": "WfErrorEnvelope"
      }
     ]
    }
   ]
  }
 ],
 "schemas": [
  {
   "name": "A2ACanonicalCard",
   "type": "object",
   "required": [
    "name",
    "version",
    "url",
    "capabilities",
    "skills"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "version",
     "type": "string",
     "ref": null
    },
    {
     "name": "provider",
     "type": "object",
     "ref": null
    },
    {
     "name": "url",
     "type": "string",
     "ref": null
    },
    {
     "name": "capabilities",
     "type": "object",
     "ref": null
    },
    {
     "name": "skills",
     "type": "array",
     "ref": null
    },
    {
     "name": "authentication",
     "type": "object",
     "ref": null
    },
    {
     "name": "preferredTransport",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "A2ACapabilities",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "streaming",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "push_notifications",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "state_transition_history",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "A2ACard",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "version",
     "type": "string",
     "ref": null
    },
    {
     "name": "provider",
     "type": "object",
     "ref": null
    },
    {
     "name": "endpoint_url",
     "type": "string",
     "ref": null
    },
    {
     "name": "capabilities",
     "type": "A2ACapabilities",
     "ref": "A2ACapabilities"
    },
    {
     "name": "skills",
     "type": "array",
     "ref": null
    },
    {
     "name": "auth_scheme",
     "type": "string",
     "ref": null
    },
    {
     "name": "preferred_transport",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "A2ACardEnvelope",
   "type": "object",
   "required": [
    "agent_card"
   ],
   "properties": [
    {
     "name": "agent_card",
     "type": "A2ACard",
     "ref": "A2ACard"
    }
   ]
  },
  {
   "name": "A2ACardInput",
   "type": "object",
   "required": [
    "agent_id",
    "name",
    "version"
   ],
   "properties": [
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "version",
     "type": "string",
     "ref": null
    },
    {
     "name": "provider",
     "type": "object",
     "ref": null
    },
    {
     "name": "endpoint_url",
     "type": "string",
     "ref": null
    },
    {
     "name": "capabilities",
     "type": "A2ACapabilities",
     "ref": "A2ACapabilities"
    },
    {
     "name": "skills",
     "type": "array",
     "ref": null
    },
    {
     "name": "auth_scheme",
     "type": "string",
     "ref": null
    },
    {
     "name": "preferred_transport",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "A2ACardListEnvelope",
   "type": "object",
   "required": [
    "agent_cards"
   ],
   "properties": [
    {
     "name": "agent_cards",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "A2ACardPatch",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "version",
     "type": "string",
     "ref": null
    },
    {
     "name": "provider",
     "type": "object",
     "ref": null
    },
    {
     "name": "endpoint_url",
     "type": "string",
     "ref": null
    },
    {
     "name": "capabilities",
     "type": "A2ACapabilities",
     "ref": "A2ACapabilities"
    },
    {
     "name": "skills",
     "type": "array",
     "ref": null
    },
    {
     "name": "auth_scheme",
     "type": "string",
     "ref": null
    },
    {
     "name": "preferred_transport",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "A2ADelegateRequest",
   "type": "object",
   "required": [
    "message"
   ],
   "properties": [
    {
     "name": "card_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "card_url",
     "type": "string",
     "ref": null
    },
    {
     "name": "message",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "A2ADelegateResult",
   "type": "object",
   "required": [
    "status",
    "http_status",
    "latency_ms"
   ],
   "properties": [
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "http_status",
     "type": "integer",
     "ref": null
    },
    {
     "name": "latency_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "card_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "endpoint",
     "type": "string",
     "ref": null
    },
    {
     "name": "verdict",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "A2ADelegationDenied",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    },
    {
     "name": "verdict",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "A2ADeletedEnvelope",
   "type": "object",
   "required": [
    "deleted",
    "id"
   ],
   "properties": [
    {
     "name": "deleted",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "A2AError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "A2AMessageRequest",
   "type": "object",
   "required": [
    "message"
   ],
   "properties": [
    {
     "name": "message",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "A2AMessageResult",
   "type": "object",
   "required": [
    "status"
   ],
   "properties": [
    {
     "name": "card_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "task_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "A2ASkill",
   "type": "object",
   "required": [
    "id"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "tags",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "AdversarialPack",
   "type": "object",
   "required": [
    "id",
    "name",
    "version",
    "scenarios"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "version",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "scenarios",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "AdversarialResult",
   "type": "object",
   "required": [
    "suite",
    "scenario_id",
    "kind",
    "outcome",
    "observed_action",
    "evidence",
    "severity",
    "expected_control"
   ],
   "properties": [
    {
     "name": "suite",
     "type": "string",
     "ref": null
    },
    {
     "name": "pack_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "pack_version",
     "type": "string",
     "ref": null
    },
    {
     "name": "scenario_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "kind",
     "type": "string",
     "ref": null
    },
    {
     "name": "outcome",
     "type": "string",
     "ref": null
    },
    {
     "name": "policy_decision",
     "type": "object",
     "ref": null
    },
    {
     "name": "observed_action",
     "type": "string",
     "ref": null
    },
    {
     "name": "evidence",
     "type": "string",
     "ref": null
    },
    {
     "name": "severity",
     "type": "string",
     "ref": null
    },
    {
     "name": "expected_control",
     "type": "string",
     "ref": null
    },
    {
     "name": "duration_ms",
     "type": "number",
     "ref": null
    },
    {
     "name": "error",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AdversarialScenario",
   "type": "object",
   "required": [
    "id",
    "name",
    "kind",
    "setup",
    "attack_payload",
    "expected_control",
    "severity"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "kind",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "setup",
     "type": "AdversarialSetup",
     "ref": "AdversarialSetup"
    },
    {
     "name": "attack_payload",
     "type": "string",
     "ref": null
    },
    {
     "name": "expected_control",
     "type": "string",
     "ref": null
    },
    {
     "name": "severity",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AdversarialSetup",
   "type": "object",
   "required": [
    "organization_id"
   ],
   "properties": [
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "tool_name",
     "type": "string",
     "ref": null
    },
    {
     "name": "foreign_organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "foreign_resource_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "target",
     "type": "string",
     "ref": null
    },
    {
     "name": "via_untrusted_tool_output",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "Agent",
   "type": "object",
   "required": [
    "ID",
    "OrganizationID",
    "Name",
    "Instructions",
    "Model",
    "Status",
    "CreatedAt",
    "UpdatedAt"
   ],
   "properties": [
    {
     "name": "ID",
     "type": "string",
     "ref": null
    },
    {
     "name": "OrganizationID",
     "type": "string",
     "ref": null
    },
    {
     "name": "Name",
     "type": "string",
     "ref": null
    },
    {
     "name": "Description",
     "type": "string",
     "ref": null
    },
    {
     "name": "Instructions",
     "type": "string",
     "ref": null
    },
    {
     "name": "Model",
     "type": "string",
     "ref": null
    },
    {
     "name": "Status",
     "type": "string",
     "ref": null
    },
    {
     "name": "CurrentVersionID",
     "type": "string",
     "ref": null
    },
    {
     "name": "CreatedAt",
     "type": "string",
     "ref": null
    },
    {
     "name": "UpdatedAt",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AgentConfigVersion",
   "type": "object",
   "required": [
    "version",
    "snapshot",
    "published_at",
    "published_by",
    "status"
   ],
   "properties": [
    {
     "name": "version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "snapshot",
     "type": "AgentSnapshot",
     "ref": "AgentSnapshot"
    },
    {
     "name": "published_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "published_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AgentExportAgent",
   "type": "object",
   "required": [
    "name",
    "description",
    "instructions",
    "model",
    "tools"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "instructions",
     "type": "string",
     "ref": null
    },
    {
     "name": "model",
     "type": "string",
     "ref": null
    },
    {
     "name": "tools",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "AgentExportBundle",
   "type": "object",
   "required": [
    "schema_version",
    "agent"
   ],
   "properties": [
    {
     "name": "schema_version",
     "type": "string",
     "ref": null
    },
    {
     "name": "exported_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent",
     "type": "AgentExportAgent",
     "ref": "AgentExportAgent"
    }
   ]
  },
  {
   "name": "AgentKey",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "key_prefix",
     "type": "string",
     "ref": null
    },
    {
     "name": "tool_allow_list",
     "type": "array",
     "ref": null
    },
    {
     "name": "expires_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "revoked_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "last_used_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AgentKeyCreate",
   "type": "object",
   "required": [
    "name"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "tool_allow_list",
     "type": "array",
     "ref": null
    },
    {
     "name": "expires_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AgentKeyError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "AgentKeyUpdate",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "tool_allow_list",
     "type": "array",
     "ref": null
    },
    {
     "name": "expires_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AgentMessage",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "from_agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "to_agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "message_type",
     "type": "string",
     "ref": null
    },
    {
     "name": "payload",
     "type": "-",
     "ref": null
    },
    {
     "name": "timestamp",
     "type": "string",
     "ref": null
    },
    {
     "name": "message_id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AgentSnapshot",
   "type": "object",
   "required": [
    "name",
    "description",
    "instructions",
    "model",
    "status"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "instructions",
     "type": "string",
     "ref": null
    },
    {
     "name": "model",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AgentVersion",
   "type": "object",
   "required": [
    "ID",
    "AgentID",
    "Version",
    "Instructions",
    "Model",
    "CreatedAt"
   ],
   "properties": [
    {
     "name": "ID",
     "type": "string",
     "ref": null
    },
    {
     "name": "AgentID",
     "type": "string",
     "ref": null
    },
    {
     "name": "Version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "Instructions",
     "type": "string",
     "ref": null
    },
    {
     "name": "Model",
     "type": "string",
     "ref": null
    },
    {
     "name": "CreatedAt",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AgentVersionsResponse",
   "type": "object",
   "required": [
    "versions"
   ],
   "properties": [
    {
     "name": "versions",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "AglAgent",
   "type": "object",
   "required": [
    "ID",
    "OrganizationID",
    "Name",
    "Model",
    "Status",
    "CreatedAt",
    "UpdatedAt"
   ],
   "properties": [
    {
     "name": "ID",
     "type": "string",
     "ref": null
    },
    {
     "name": "OrganizationID",
     "type": "string",
     "ref": null
    },
    {
     "name": "Name",
     "type": "string",
     "ref": null
    },
    {
     "name": "Description",
     "type": "string",
     "ref": null
    },
    {
     "name": "Instructions",
     "type": "string",
     "ref": null
    },
    {
     "name": "Model",
     "type": "string",
     "ref": null
    },
    {
     "name": "Status",
     "type": "string",
     "ref": null
    },
    {
     "name": "CurrentVersionID",
     "type": "string",
     "ref": null
    },
    {
     "name": "CreatedAt",
     "type": "string",
     "ref": null
    },
    {
     "name": "UpdatedAt",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AglDeleteAgentResponse",
   "type": "object",
   "required": [
    "deleted",
    "id"
   ],
   "properties": [
    {
     "name": "deleted",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AglErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "AglUpdateAgentRequest",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "instructions",
     "type": "string",
     "ref": null
    },
    {
     "name": "system_prompt",
     "type": "string",
     "ref": null
    },
    {
     "name": "model",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ApiKeyCreatedResponse",
   "type": "object",
   "required": [
    "api_key",
    "value"
   ],
   "properties": [
    {
     "name": "api_key",
     "type": "ApiKeyMetadata",
     "ref": "ApiKeyMetadata"
    },
    {
     "name": "value",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ApiKeyCreateRequest",
   "type": "object",
   "required": [
    "name"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ApiKeyErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ApiKeyListResponse",
   "type": "object",
   "required": [
    "api_keys"
   ],
   "properties": [
    {
     "name": "api_keys",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "ApiKeyMetadata",
   "type": "object",
   "required": [
    "id",
    "name",
    "prefix",
    "created_by",
    "created_at",
    "revoked"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "prefix",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "revoked",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "ApiKeyRevokeResponse",
   "type": "object",
   "required": [
    "revoked"
   ],
   "properties": [
    {
     "name": "revoked",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "Approval",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "workflow_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "resource",
     "type": "string",
     "ref": null
    },
    {
     "name": "action",
     "type": "string",
     "ref": null
    },
    {
     "name": "reason",
     "type": "string",
     "ref": null
    },
    {
     "name": "risk",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "requester",
     "type": "string",
     "ref": null
    },
    {
     "name": "approver",
     "type": "string",
     "ref": null
    },
    {
     "name": "decision_reason",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "decided_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "require_dual",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "first_approver",
     "type": "string",
     "ref": null
    },
    {
     "name": "first_decision",
     "type": "string",
     "ref": null
    },
    {
     "name": "first_decided_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "delegated_to",
     "type": "string",
     "ref": null
    },
    {
     "name": "delegated_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "delegation_note",
     "type": "string",
     "ref": null
    },
    {
     "name": "delegation_expires_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "escalated_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "escalation_reason",
     "type": "string",
     "ref": null
    },
    {
     "name": "escalation_deadline",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ApprovalDelegation",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "approval_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "from_user",
     "type": "string",
     "ref": null
    },
    {
     "name": "to_user",
     "type": "string",
     "ref": null
    },
    {
     "name": "note",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "expires_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ApprovalDelegationInput",
   "type": "object",
   "required": [
    "to_user"
   ],
   "properties": [
    {
     "name": "to_user",
     "type": "string",
     "ref": null
    },
    {
     "name": "note",
     "type": "string",
     "ref": null
    },
    {
     "name": "expires_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ApprovalEscalationPolicy",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "target_status",
     "type": "string",
     "ref": null
    },
    {
     "name": "sla_minutes",
     "type": "integer",
     "ref": null
    },
    {
     "name": "updated_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ApprovalEscalationPolicyInput",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "target_status",
     "type": "string",
     "ref": null
    },
    {
     "name": "sla_minutes",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "ApprovalListResponse",
   "type": "object",
   "required": [
    "approvals"
   ],
   "properties": [
    {
     "name": "approvals",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "ApprovalRisk",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "ApprovalsEnterpriseError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ApprovalStatus",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "AudChainReport",
   "type": "object",
   "required": [
    "valid",
    "length"
   ],
   "properties": [
    {
     "name": "valid",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "length",
     "type": "integer",
     "ref": null
    },
    {
     "name": "head_hash",
     "type": "string",
     "ref": null
    },
    {
     "name": "expected_head_hash",
     "type": "string",
     "ref": null
    },
    {
     "name": "broken_at_index",
     "type": "integer",
     "ref": null
    },
    {
     "name": "unstamped",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "AudError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "AuditErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "AuditEvent",
   "type": "object",
   "required": [
    "id",
    "actor",
    "action",
    "created_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "actor",
     "type": "string",
     "ref": null
    },
    {
     "name": "action",
     "type": "string",
     "ref": null
    },
    {
     "name": "resource",
     "type": "string",
     "ref": null
    },
    {
     "name": "metadata",
     "type": "object",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AuditEventsResponse",
   "type": "object",
   "required": [
    "events",
    "next_cursor"
   ],
   "properties": [
    {
     "name": "events",
     "type": "array",
     "ref": null
    },
    {
     "name": "next_cursor",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "AuthLifecycleErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "AuthLifecycleRefreshResponse",
   "type": "object",
   "required": [
    "token"
   ],
   "properties": [
    {
     "name": "token",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchAgentDeployRequest",
   "type": "object",
   "required": [
    "agent_ids"
   ],
   "properties": [
    {
     "name": "agent_ids",
     "type": "array",
     "ref": null
    },
    {
     "name": "version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "environment",
     "type": "string",
     "ref": null
    },
    {
     "name": "max_concurrency",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchAgentDeployResponse",
   "type": "-",
   "required": [],
   "properties": []
  },
  {
   "name": "BatchAgentPatch",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "instructions",
     "type": "string",
     "ref": null
    },
    {
     "name": "model",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchAgentUpdateRequest",
   "type": "object",
   "required": [
    "agent_ids",
    "patch"
   ],
   "properties": [
    {
     "name": "agent_ids",
     "type": "array",
     "ref": null
    },
    {
     "name": "patch",
     "type": "BatchAgentPatch",
     "ref": "BatchAgentPatch"
    },
    {
     "name": "max_concurrency",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchAgentUpdateResponse",
   "type": "-",
   "required": [],
   "properties": []
  },
  {
   "name": "BatchErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchItem",
   "type": "object",
   "required": [
    "ref",
    "status"
   ],
   "properties": [
    {
     "name": "ref",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "error",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "task_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "workflow_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "deployment_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "environment",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "key_version",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchRecord",
   "type": "object",
   "required": [
    "batch_id",
    "organization_id",
    "type",
    "status",
    "summary",
    "total",
    "completed",
    "items",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "batch_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "max_concurrency",
     "type": "integer",
     "ref": null
    },
    {
     "name": "summary",
     "type": "BatchSummary",
     "ref": "BatchSummary"
    },
    {
     "name": "total",
     "type": "integer",
     "ref": null
    },
    {
     "name": "completed",
     "type": "integer",
     "ref": null
    },
    {
     "name": "items",
     "type": "array",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "completed_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchRunCancelRequest",
   "type": "object",
   "required": [
    "run_ids"
   ],
   "properties": [
    {
     "name": "run_ids",
     "type": "array",
     "ref": null
    },
    {
     "name": "max_concurrency",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchRunCancelResponse",
   "type": "-",
   "required": [],
   "properties": []
  },
  {
   "name": "BatchRunsRequest",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_ids",
     "type": "array",
     "ref": null
    },
    {
     "name": "input",
     "type": "string",
     "ref": null
    },
    {
     "name": "inputs",
     "type": "array",
     "ref": null
    },
    {
     "name": "max_concurrency",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchRunsResponse",
   "type": "-",
   "required": [],
   "properties": []
  },
  {
   "name": "BatchSecretsRequest",
   "type": "object",
   "required": [
    "secrets"
   ],
   "properties": [
    {
     "name": "secrets",
     "type": "array",
     "ref": null
    },
    {
     "name": "max_concurrency",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchSecretsResponse",
   "type": "-",
   "required": [],
   "properties": []
  },
  {
   "name": "BatchSubmission",
   "type": "object",
   "required": [
    "batch_id",
    "status",
    "summary",
    "items"
   ],
   "properties": [
    {
     "name": "batch_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "summary",
     "type": "BatchSummary",
     "ref": "BatchSummary"
    },
    {
     "name": "items",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchSummary",
   "type": "object",
   "required": [
    "total",
    "succeeded",
    "failed",
    "cancelled"
   ],
   "properties": [
    {
     "name": "total",
     "type": "integer",
     "ref": null
    },
    {
     "name": "succeeded",
     "type": "integer",
     "ref": null
    },
    {
     "name": "failed",
     "type": "integer",
     "ref": null
    },
    {
     "name": "cancelled",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchWorkflowTriggerRequest",
   "type": "object",
   "required": [
    "inputs"
   ],
   "properties": [
    {
     "name": "inputs",
     "type": "array",
     "ref": null
    },
    {
     "name": "max_concurrency",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "BatchWorkflowTriggerResponse",
   "type": "-",
   "required": [],
   "properties": []
  },
  {
   "name": "BillCancelRequest",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "immediate",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "BillErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "BillInvoice",
   "type": "object",
   "required": [
    "id",
    "subscription_id",
    "period_start",
    "period_end",
    "subtotal_cents",
    "currency",
    "status"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "subscription_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "period_start",
     "type": "string",
     "ref": null
    },
    {
     "name": "period_end",
     "type": "string",
     "ref": null
    },
    {
     "name": "subtotal_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "currency",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "BillInvoiceLine",
   "type": "object",
   "required": [
    "id",
    "source",
    "quantity",
    "amount_cents"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "source",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "quantity",
     "type": "integer",
     "ref": null
    },
    {
     "name": "amount_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "refs",
     "type": "object",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "BillInvoiceList",
   "type": "object",
   "required": [
    "invoices"
   ],
   "properties": [
    {
     "name": "invoices",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "BillInvoiceResponse",
   "type": "object",
   "required": [
    "invoice"
   ],
   "properties": [
    {
     "name": "invoice",
     "type": "-",
     "ref": null
    }
   ]
  },
  {
   "name": "BillPlan",
   "type": "object",
   "required": [
    "id",
    "name",
    "price_cents",
    "currency",
    "included_quota"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "price_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "currency",
     "type": "string",
     "ref": null
    },
    {
     "name": "included_quota",
     "type": "integer",
     "ref": null
    },
    {
     "name": "metadata",
     "type": "object",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "BillPlanList",
   "type": "object",
   "required": [
    "plans"
   ],
   "properties": [
    {
     "name": "plans",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "BillQuotaStatus",
   "type": "object",
   "required": [
    "subscription_id",
    "status",
    "included_runs",
    "unlimited",
    "consumed_runs",
    "remaining_runs",
    "exceeded",
    "period_start",
    "period_end"
   ],
   "properties": [
    {
     "name": "subscription_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "included_runs",
     "type": "integer",
     "ref": null
    },
    {
     "name": "unlimited",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "consumed_runs",
     "type": "integer",
     "ref": null
    },
    {
     "name": "remaining_runs",
     "type": "integer",
     "ref": null
    },
    {
     "name": "exceeded",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "period_start",
     "type": "string",
     "ref": null
    },
    {
     "name": "period_end",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "BillSubscribeRequest",
   "type": "object",
   "required": [
    "plan_id"
   ],
   "properties": [
    {
     "name": "plan_id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "BillSubscription",
   "type": "object",
   "required": [
    "id",
    "plan_id",
    "status",
    "period_start",
    "period_end",
    "cancel_at_period_end"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "plan_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "period_start",
     "type": "string",
     "ref": null
    },
    {
     "name": "period_end",
     "type": "string",
     "ref": null
    },
    {
     "name": "cancel_at_period_end",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "canceled_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "BillSubscriptionResponse",
   "type": "object",
   "required": [
    "subscription"
   ],
   "properties": [
    {
     "name": "subscription",
     "type": "BillSubscription",
     "ref": "BillSubscription"
    }
   ]
  },
  {
   "name": "BillSubscriptionWithQuota",
   "type": "object",
   "required": [
    "subscription",
    "quota"
   ],
   "properties": [
    {
     "name": "subscription",
     "type": "BillSubscription",
     "ref": "BillSubscription"
    },
    {
     "name": "quota",
     "type": "BillQuotaStatus",
     "ref": "BillQuotaStatus"
    }
   ]
  },
  {
   "name": "Budget",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "scope",
     "type": "string",
     "ref": null
    },
    {
     "name": "ref_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "period",
     "type": "string",
     "ref": null
    },
    {
     "name": "limit_runs",
     "type": "integer",
     "ref": null
    },
    {
     "name": "limit_cost_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "is_active",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "BudgetDecision",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "allowed",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "reason",
     "type": "string",
     "ref": null
    },
    {
     "name": "denied_budget_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "denied_scope",
     "type": "string",
     "ref": null
    },
    {
     "name": "message",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "BudgetError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "BudgetInput",
   "type": "object",
   "required": [
    "scope"
   ],
   "properties": [
    {
     "name": "scope",
     "type": "string",
     "ref": null
    },
    {
     "name": "ref_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "period",
     "type": "string",
     "ref": null
    },
    {
     "name": "limit_runs",
     "type": "integer",
     "ref": null
    },
    {
     "name": "limit_cost_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "is_active",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "CanaryConfigRequest",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "canary_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "canary_weight",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "CanaryDeploymentHealth",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error_rate",
     "type": "number",
     "ref": null
    },
    {
     "name": "last_check_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "superseded_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "error",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "CanaryDeploymentResponse",
   "type": "object",
   "required": [
    "deployment"
   ],
   "properties": [
    {
     "name": "deployment",
     "type": "CanaryDeploymentView",
     "ref": "CanaryDeploymentView"
    }
   ]
  },
  {
   "name": "CanaryDeploymentView",
   "type": "object",
   "required": [
    "id",
    "agent_id",
    "version",
    "environment",
    "status",
    "health",
    "created_at",
    "updated_at",
    "canary_version",
    "canary_weight"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "environment",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "health",
     "type": "CanaryDeploymentHealth",
     "ref": "CanaryDeploymentHealth"
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "canary_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "canary_weight",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "CanaryErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "CnpCanaryConfigRequest",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "canary_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "canary_weight",
     "type": "integer",
     "ref": null
    },
    {
     "name": "canary_policy",
     "type": "-",
     "ref": null
    }
   ]
  },
  {
   "name": "CnpCanaryDecision",
   "type": "object",
   "required": [
    "action",
    "reason",
    "decided_at",
    "runs_counted",
    "pass_rate",
    "p95_latency_ms",
    "avg_cost_cents",
    "policy"
   ],
   "properties": [
    {
     "name": "action",
     "type": "string",
     "ref": null
    },
    {
     "name": "reason",
     "type": "string",
     "ref": null
    },
    {
     "name": "decided_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "runs_counted",
     "type": "integer",
     "ref": null
    },
    {
     "name": "pass_rate",
     "type": "number",
     "ref": null
    },
    {
     "name": "p95_latency_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "avg_cost_cents",
     "type": "number",
     "ref": null
    },
    {
     "name": "policy",
     "type": "CnpPromotionPolicy",
     "ref": "CnpPromotionPolicy"
    }
   ]
  },
  {
   "name": "CnpCanaryStatus",
   "type": "object",
   "required": [
    "agent_id",
    "environment",
    "deployment_id",
    "stable_version",
    "canary_version",
    "canary_weight",
    "canary_active"
   ],
   "properties": [
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "environment",
     "type": "string",
     "ref": null
    },
    {
     "name": "deployment_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "stable_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "canary_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "canary_weight",
     "type": "integer",
     "ref": null
    },
    {
     "name": "canary_active",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "window_start",
     "type": "string",
     "ref": null
    },
    {
     "name": "policy",
     "type": "-",
     "ref": null
    },
    {
     "name": "decision",
     "type": "-",
     "ref": null
    },
    {
     "name": "stats",
     "type": "-",
     "ref": null
    }
   ]
  },
  {
   "name": "CnpCanaryStatusResponse",
   "type": "object",
   "required": [
    "canary_status"
   ],
   "properties": [
    {
     "name": "canary_status",
     "type": "CnpCanaryStatus",
     "ref": "CnpCanaryStatus"
    }
   ]
  },
  {
   "name": "CnpDeploymentResponse",
   "type": "object",
   "required": [
    "deployment"
   ],
   "properties": [
    {
     "name": "deployment",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "CnpErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "CnpPromotionPolicy",
   "type": "object",
   "required": [
    "min_pass_rate",
    "min_canary_runs",
    "max_p95_latency_ms",
    "max_cost_per_run_cents"
   ],
   "properties": [
    {
     "name": "min_pass_rate",
     "type": "number",
     "ref": null
    },
    {
     "name": "min_canary_runs",
     "type": "integer",
     "ref": null
    },
    {
     "name": "max_p95_latency_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "max_cost_per_run_cents",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "CnpSampleStats",
   "type": "object",
   "required": [
    "runs_counted",
    "cases_counted",
    "passed_cases",
    "pass_rate",
    "p95_latency_ms",
    "avg_cost_cents"
   ],
   "properties": [
    {
     "name": "runs_counted",
     "type": "integer",
     "ref": null
    },
    {
     "name": "cases_counted",
     "type": "integer",
     "ref": null
    },
    {
     "name": "passed_cases",
     "type": "integer",
     "ref": null
    },
    {
     "name": "pass_rate",
     "type": "number",
     "ref": null
    },
    {
     "name": "p95_latency_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "avg_cost_cents",
     "type": "number",
     "ref": null
    }
   ]
  },
  {
   "name": "CodeExecInput",
   "type": "object",
   "required": [
    "language",
    "code"
   ],
   "properties": [
    {
     "name": "language",
     "type": "string",
     "ref": null
    },
    {
     "name": "code",
     "type": "string",
     "ref": null
    },
    {
     "name": "timeout_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "estimated_cost_cents",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "CodeExecOutput",
   "type": "object",
   "required": [
    "language",
    "stdout",
    "stderr",
    "exit_code",
    "truncated",
    "duration_ms"
   ],
   "properties": [
    {
     "name": "language",
     "type": "string",
     "ref": null
    },
    {
     "name": "stdout",
     "type": "string",
     "ref": null
    },
    {
     "name": "stderr",
     "type": "string",
     "ref": null
    },
    {
     "name": "exit_code",
     "type": "integer",
     "ref": null
    },
    {
     "name": "truncated",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "duration_ms",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "ComControl",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "satisfied_by",
     "type": "array",
     "ref": null
    },
    {
     "name": "frameworks",
     "type": "array",
     "ref": null
    },
    {
     "name": "evidence",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "ComControlsReport",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "generated_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "catalog_version",
     "type": "string",
     "ref": null
    },
    {
     "name": "frameworks",
     "type": "array",
     "ref": null
    },
    {
     "name": "domains",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "ComDomain",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "controls",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "ComError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ComEvidence",
   "type": "object",
   "required": [
    "kind",
    "source"
   ],
   "properties": [
    {
     "name": "kind",
     "type": "string",
     "ref": null
    },
    {
     "name": "source",
     "type": "string",
     "ref": null
    },
    {
     "name": "value",
     "type": "-",
     "ref": null
    },
    {
     "name": "surface",
     "type": "string",
     "ref": null
    },
    {
     "name": "note",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ComTechDoc",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "agent",
     "type": "object",
     "ref": null
    },
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "generated_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "framework_reference",
     "type": "string",
     "ref": null
    },
    {
     "name": "versions",
     "type": "array",
     "ref": null
    },
    {
     "name": "evaluations",
     "type": "object",
     "ref": null
    },
    {
     "name": "deployments",
     "type": "array",
     "ref": null
    },
    {
     "name": "tool_surface",
     "type": "object",
     "ref": null
    },
    {
     "name": "policy_bindings",
     "type": "array",
     "ref": null
    },
    {
     "name": "audit_posture",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ConConfig",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "auth_style",
     "type": "string",
     "ref": null
    },
    {
     "name": "headers",
     "type": "object",
     "ref": null
    },
    {
     "name": "api_key_header",
     "type": "string",
     "ref": null
    },
    {
     "name": "api_key_prefix",
     "type": "string",
     "ref": null
    },
    {
     "name": "username",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ConConnector",
   "type": "object",
   "required": [
    "id",
    "name",
    "type",
    "base_url",
    "config",
    "secret_ref",
    "status",
    "last_check_at",
    "last_check_status",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "base_url",
     "type": "string",
     "ref": null
    },
    {
     "name": "config",
     "type": "ConConfig",
     "ref": "ConConfig"
    },
    {
     "name": "secret_ref",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "last_check_at",
     "type": "['string', 'null']",
     "ref": null
    },
    {
     "name": "last_check_status",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ConCreateRequest",
   "type": "object",
   "required": [
    "name",
    "type",
    "base_url"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "base_url",
     "type": "string",
     "ref": null
    },
    {
     "name": "auth_style",
     "type": "string",
     "ref": null
    },
    {
     "name": "headers",
     "type": "object",
     "ref": null
    },
    {
     "name": "api_key_header",
     "type": "string",
     "ref": null
    },
    {
     "name": "api_key_prefix",
     "type": "string",
     "ref": null
    },
    {
     "name": "username",
     "type": "string",
     "ref": null
    },
    {
     "name": "secret_ref",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ConDeleteResponse",
   "type": "object",
   "required": [
    "deleted"
   ],
   "properties": [
    {
     "name": "deleted",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "ConErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ConListResponse",
   "type": "object",
   "required": [
    "connectors"
   ],
   "properties": [
    {
     "name": "connectors",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "ConResponse",
   "type": "object",
   "required": [
    "connector"
   ],
   "properties": [
    {
     "name": "connector",
     "type": "ConConnector",
     "ref": "ConConnector"
    }
   ]
  },
  {
   "name": "ConTestResponse",
   "type": "object",
   "required": [
    "test"
   ],
   "properties": [
    {
     "name": "test",
     "type": "ConTestResult",
     "ref": "ConTestResult"
    }
   ]
  },
  {
   "name": "ConTestResult",
   "type": "object",
   "required": [
    "connector_id",
    "status",
    "status_code",
    "latency_ms",
    "checked_at"
   ],
   "properties": [
    {
     "name": "connector_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "status_code",
     "type": "integer",
     "ref": null
    },
    {
     "name": "latency_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "error",
     "type": "string",
     "ref": null
    },
    {
     "name": "checked_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "CreateAgentRequest",
   "type": "object",
   "required": [
    "name",
    "instructions",
    "model"
   ],
   "properties": [
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "instructions",
     "type": "string",
     "ref": null
    },
    {
     "name": "model",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "CreateDeploymentRequest",
   "type": "object",
   "required": [
    "agent_id",
    "version",
    "environment"
   ],
   "properties": [
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "environment",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "CreateRunRequest",
   "type": "object",
   "required": [
    "agent_id",
    "input"
   ],
   "properties": [
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "input",
     "type": "string",
     "ref": null
    },
    {
     "name": "environment",
     "type": "string",
     "ref": null
    },
    {
     "name": "estimated_cost_cents",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "CreateRunResponse",
   "type": "object",
   "required": [
    "run_id",
    "status"
   ],
   "properties": [
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "CreateWebhookRequest",
   "type": "object",
   "required": [
    "url"
   ],
   "properties": [
    {
     "name": "url",
     "type": "string",
     "ref": null
    },
    {
     "name": "events",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "CreateWebhookResponse",
   "type": "object",
   "required": [
    "webhook",
    "secret"
   ],
   "properties": [
    {
     "name": "webhook",
     "type": "Webhook",
     "ref": "Webhook"
    },
    {
     "name": "secret",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "CreateWorkflowRequest",
   "type": "object",
   "required": [
    "name",
    "dsl"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "dsl",
     "type": "WorkflowDSL",
     "ref": "WorkflowDSL"
    }
   ]
  },
  {
   "name": "CurrentVersionResponse",
   "type": "object",
   "required": [
    "current_version"
   ],
   "properties": [
    {
     "name": "current_version",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "DatabaseQueryInput",
   "type": "object",
   "required": [
    "connection",
    "sql"
   ],
   "properties": [
    {
     "name": "connection",
     "type": "string",
     "ref": null
    },
    {
     "name": "sql",
     "type": "string",
     "ref": null
    },
    {
     "name": "params",
     "type": "object",
     "ref": null
    },
    {
     "name": "row_limit",
     "type": "integer",
     "ref": null
    },
    {
     "name": "readonly",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "timeout_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "estimated_cost_cents",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "DatabaseQueryOutput",
   "type": "object",
   "required": [
    "columns",
    "rows",
    "row_count",
    "truncated"
   ],
   "properties": [
    {
     "name": "columns",
     "type": "array",
     "ref": null
    },
    {
     "name": "rows",
     "type": "array",
     "ref": null
    },
    {
     "name": "row_count",
     "type": "integer",
     "ref": null
    },
    {
     "name": "truncated",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "DecideApprovalRequest",
   "type": "object",
   "required": [
    "decision"
   ],
   "properties": [
    {
     "name": "decision",
     "type": "string",
     "ref": null
    },
    {
     "name": "reason",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "DelegateAccepted",
   "type": "object",
   "required": [
    "delegation_id",
    "child_run_id",
    "status",
    "depth"
   ],
   "properties": [
    {
     "name": "delegation_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "child_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "depth",
     "type": "integer",
     "ref": null
    },
    {
     "name": "approval_id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "DelegateRequest",
   "type": "object",
   "required": [
    "from_agent_id",
    "to_agent_id",
    "task"
   ],
   "properties": [
    {
     "name": "from_agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "to_agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "task",
     "type": "string",
     "ref": null
    },
    {
     "name": "parent_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "require_approval",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "depth",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "DelegationFailedError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    },
    {
     "name": "delegation_id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "DelegationRecord",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "from_agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "to_agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "task",
     "type": "string",
     "ref": null
    },
    {
     "name": "parent_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "child_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "depth",
     "type": "integer",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "approval_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "result",
     "type": "string",
     "ref": null
    },
    {
     "name": "error",
     "type": "string",
     "ref": null
    },
    {
     "name": "tokens",
     "type": "OrchestrationUsage",
     "ref": "OrchestrationUsage"
    },
    {
     "name": "response_delivered",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "completed_at",
     "type": "-",
     "ref": null
    }
   ]
  },
  {
   "name": "DeleteScheduleResponse",
   "type": "object",
   "required": [
    "deleted"
   ],
   "properties": [
    {
     "name": "deleted",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "Deployment",
   "type": "object",
   "required": [
    "id",
    "agent_id",
    "version",
    "environment",
    "status",
    "health",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "environment",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "health",
     "type": "DeploymentHealth",
     "ref": "DeploymentHealth"
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "DeploymentHealth",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error_rate",
     "type": "number",
     "ref": null
    },
    {
     "name": "last_check_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "superseded_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "error",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "DeploymentResponse",
   "type": "object",
   "required": [
    "deployment"
   ],
   "properties": [
    {
     "name": "deployment",
     "type": "Deployment",
     "ref": "Deployment"
    }
   ]
  },
  {
   "name": "DeploymentsResponse",
   "type": "object",
   "required": [
    "deployments"
   ],
   "properties": [
    {
     "name": "deployments",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "DSLEdge",
   "type": "object",
   "required": [
    "from",
    "to"
   ],
   "properties": [
    {
     "name": "from",
     "type": "string",
     "ref": null
    },
    {
     "name": "to",
     "type": "string",
     "ref": null
    },
    {
     "name": "condition",
     "type": "DSLEdgeCondition",
     "ref": "DSLEdgeCondition"
    }
   ]
  },
  {
   "name": "DSLEdgeCondition",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "DSLNode",
   "type": "object",
   "required": [
    "id",
    "type"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "DSLNodeType",
     "ref": "DSLNodeType"
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "config",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "DSLNodeType",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "EmbErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "EmbFusionWeights",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "keyword_weight",
     "type": "number",
     "ref": null
    },
    {
     "name": "vector_weight",
     "type": "number",
     "ref": null
    },
    {
     "name": "fusion",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "EmbSearchRequest",
   "type": "object",
   "required": [
    "query"
   ],
   "properties": [
    {
     "name": "query",
     "type": "string",
     "ref": null
    },
    {
     "name": "k",
     "type": "integer",
     "ref": null
    },
    {
     "name": "mode",
     "type": "string",
     "ref": null
    },
    {
     "name": "weights",
     "type": "EmbFusionWeights",
     "ref": "EmbFusionWeights"
    }
   ]
  },
  {
   "name": "EmbSearchResponse",
   "type": "object",
   "required": [
    "results"
   ],
   "properties": [
    {
     "name": "results",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "EmbSearchResult",
   "type": "object",
   "required": [
    "document_id",
    "chunk_ordinal",
    "content",
    "score",
    "citation"
   ],
   "properties": [
    {
     "name": "document_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "document_title",
     "type": "string",
     "ref": null
    },
    {
     "name": "chunk_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "chunk_ordinal",
     "type": "integer",
     "ref": null
    },
    {
     "name": "content",
     "type": "string",
     "ref": null
    },
    {
     "name": "score",
     "type": "number",
     "ref": null
    },
    {
     "name": "citation",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "Error",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "EvalCase",
   "type": "object",
   "required": [
    "scorer"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "input",
     "type": "string",
     "ref": null
    },
    {
     "name": "expected",
     "type": "string",
     "ref": null
    },
    {
     "name": "scorer",
     "type": "string",
     "ref": null
    },
    {
     "name": "params",
     "type": "EvalParams",
     "ref": "EvalParams"
    }
   ]
  },
  {
   "name": "EvalCaseComparison",
   "type": "object",
   "required": [
    "case_id",
    "baseline_passed",
    "candidate_passed"
   ],
   "properties": [
    {
     "name": "case_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "baseline_passed",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "candidate_passed",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "EvalComparison",
   "type": "object",
   "required": [
    "baseline",
    "candidate",
    "regressions",
    "improvements"
   ],
   "properties": [
    {
     "name": "baseline",
     "type": "EvalSummary",
     "ref": "EvalSummary"
    },
    {
     "name": "candidate",
     "type": "EvalSummary",
     "ref": "EvalSummary"
    },
    {
     "name": "regressions",
     "type": "array",
     "ref": null
    },
    {
     "name": "improvements",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "EvalDataset",
   "type": "-",
   "required": [],
   "properties": []
  },
  {
   "name": "EvalDatasetSummary",
   "type": "object",
   "required": [
    "id",
    "name",
    "case_count",
    "created_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "case_count",
     "type": "integer",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "EvalErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "EvalParams",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "pattern",
     "type": "string",
     "ref": null
    },
    {
     "name": "threshold_ms",
     "type": "number",
     "ref": null
    },
    {
     "name": "threshold_cents",
     "type": "number",
     "ref": null
    }
   ]
  },
  {
   "name": "EvalResult",
   "type": "object",
   "required": [
    "case_id",
    "output",
    "passed",
    "score",
    "latency_ms",
    "cost_cents",
    "error"
   ],
   "properties": [
    {
     "name": "case_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "output",
     "type": "string",
     "ref": null
    },
    {
     "name": "passed",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "score",
     "type": "number",
     "ref": null
    },
    {
     "name": "latency_ms",
     "type": "number",
     "ref": null
    },
    {
     "name": "cost_cents",
     "type": "number",
     "ref": null
    },
    {
     "name": "error",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "EvalRunResponse",
   "type": "object",
   "required": [
    "id",
    "dataset_id",
    "agent_id",
    "status",
    "results",
    "summary"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "dataset_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "results",
     "type": "array",
     "ref": null
    },
    {
     "name": "summary",
     "type": "EvalSummary",
     "ref": "EvalSummary"
    }
   ]
  },
  {
   "name": "EvalSummary",
   "type": "object",
   "required": [
    "pass_rate",
    "avg_latency_ms",
    "total_cost_cents",
    "by_scorer"
   ],
   "properties": [
    {
     "name": "pass_rate",
     "type": "number",
     "ref": null
    },
    {
     "name": "avg_latency_ms",
     "type": "number",
     "ref": null
    },
    {
     "name": "total_cost_cents",
     "type": "number",
     "ref": null
    },
    {
     "name": "by_scorer",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "EvErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "EvEvent",
   "type": "object",
   "required": [
    "id",
    "type",
    "timestamp"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "entity_type",
     "type": "string",
     "ref": null
    },
    {
     "name": "entity_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "project_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "execution_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "trace_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "payload",
     "type": "object",
     "ref": null
    },
    {
     "name": "timestamp",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "EvEventListResponse",
   "type": "object",
   "required": [
    "events",
    "next_cursor"
   ],
   "properties": [
    {
     "name": "events",
     "type": "array",
     "ref": null
    },
    {
     "name": "next_cursor",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ExecuteWorkflowRequest",
   "type": "object",
   "required": [
    "input"
   ],
   "properties": [
    {
     "name": "input",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ExecutionResult",
   "type": "object",
   "required": [
    "workflow_run_id",
    "run_ids",
    "status"
   ],
   "properties": [
    {
     "name": "workflow_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_ids",
     "type": "array",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "FileStorageInput",
   "type": "object",
   "required": [
    "operation"
   ],
   "properties": [
    {
     "name": "operation",
     "type": "string",
     "ref": null
    },
    {
     "name": "path",
     "type": "string",
     "ref": null
    },
    {
     "name": "content",
     "type": "string",
     "ref": null
    },
    {
     "name": "content_type",
     "type": "string",
     "ref": null
    },
    {
     "name": "prefix",
     "type": "string",
     "ref": null
    },
    {
     "name": "limit",
     "type": "integer",
     "ref": null
    },
    {
     "name": "estimated_cost_cents",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "FileStorageObject",
   "type": "object",
   "required": [
    "path",
    "size",
    "content_type"
   ],
   "properties": [
    {
     "name": "path",
     "type": "string",
     "ref": null
    },
    {
     "name": "size",
     "type": "integer",
     "ref": null
    },
    {
     "name": "content_type",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "FileStorageOutput",
   "type": "object",
   "required": [
    "operation"
   ],
   "properties": [
    {
     "name": "operation",
     "type": "string",
     "ref": null
    },
    {
     "name": "path",
     "type": "string",
     "ref": null
    },
    {
     "name": "size",
     "type": "integer",
     "ref": null
    },
    {
     "name": "content_type",
     "type": "string",
     "ref": null
    },
    {
     "name": "content",
     "type": "string",
     "ref": null
    },
    {
     "name": "prefix",
     "type": "string",
     "ref": null
    },
    {
     "name": "objects",
     "type": "array",
     "ref": null
    },
    {
     "name": "count",
     "type": "integer",
     "ref": null
    },
    {
     "name": "deleted",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "GateEvidence",
   "type": "object",
   "required": [
    "suite",
    "total",
    "passed",
    "failed",
    "inconclusive",
    "pass_rate",
    "max_severity_failed",
    "max_severity_failed_rank"
   ],
   "properties": [
    {
     "name": "suite",
     "type": "string",
     "ref": null
    },
    {
     "name": "total",
     "type": "integer",
     "ref": null
    },
    {
     "name": "passed",
     "type": "integer",
     "ref": null
    },
    {
     "name": "failed",
     "type": "integer",
     "ref": null
    },
    {
     "name": "inconclusive",
     "type": "integer",
     "ref": null
    },
    {
     "name": "pass_rate",
     "type": "number",
     "ref": null
    },
    {
     "name": "max_severity_failed",
     "type": "string",
     "ref": null
    },
    {
     "name": "max_severity_failed_rank",
     "type": "integer",
     "ref": null
    },
    {
     "name": "packs",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "GatewayErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "GatewayKey",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "label",
     "type": "string",
     "ref": null
    },
    {
     "name": "prefix",
     "type": "string",
     "ref": null
    },
    {
     "name": "models",
     "type": "-",
     "ref": null
    },
    {
     "name": "monthly_limit_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "expires_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "GatewayKeyCreatedResponse",
   "type": "object",
   "required": [
    "id",
    "plaintext",
    "key"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "plaintext",
     "type": "string",
     "ref": null
    },
    {
     "name": "key",
     "type": "GatewayKey",
     "ref": "GatewayKey"
    }
   ]
  },
  {
   "name": "GatewayKeyCreateRequest",
   "type": "object",
   "required": [
    "label"
   ],
   "properties": [
    {
     "name": "label",
     "type": "string",
     "ref": null
    },
    {
     "name": "models",
     "type": "-",
     "ref": null
    },
    {
     "name": "monthly_limit_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "expires_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "GatewayKeyUpdateRequest",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "label",
     "type": "string",
     "ref": null
    },
    {
     "name": "models",
     "type": "-",
     "ref": null
    },
    {
     "name": "monthly_limit_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "expires_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "GatewayModelNotAllowedErrorBody",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "GatewaySpendLimitErrorBody",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "GatewaySpendView",
   "type": "object",
   "required": [
    "used_cents",
    "limit_cents",
    "remaining_cents",
    "month"
   ],
   "properties": [
    {
     "name": "used_cents",
     "type": "number",
     "ref": null
    },
    {
     "name": "limit_cents",
     "type": "number",
     "ref": null
    },
    {
     "name": "remaining_cents",
     "type": "number",
     "ref": null
    },
    {
     "name": "month",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "JSONTransformInput",
   "type": "object",
   "required": [
    "data",
    "ops"
   ],
   "properties": [
    {
     "name": "data",
     "type": "-",
     "ref": null
    },
    {
     "name": "ops",
     "type": "array",
     "ref": null
    },
    {
     "name": "estimated_cost_cents",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "JSONTransformOutput",
   "type": "object",
   "required": [
    "result",
    "ops_applied"
   ],
   "properties": [
    {
     "name": "result",
     "type": "-",
     "ref": null
    },
    {
     "name": "ops_applied",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "KnbCreateDocumentRequest",
   "type": "object",
   "required": [
    "title",
    "content"
   ],
   "properties": [
    {
     "name": "title",
     "type": "string",
     "ref": null
    },
    {
     "name": "content",
     "type": "string",
     "ref": null
    },
    {
     "name": "source",
     "type": "string",
     "ref": null
    },
    {
     "name": "metadata",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "KnbCreateDocumentResponse",
   "type": "object",
   "required": [
    "document"
   ],
   "properties": [
    {
     "name": "document",
     "type": "KnbDocument",
     "ref": "KnbDocument"
    },
    {
     "name": "warning",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "KnbDocument",
   "type": "object",
   "required": [
    "id",
    "title",
    "chunk_count",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "title",
     "type": "string",
     "ref": null
    },
    {
     "name": "source",
     "type": "string",
     "ref": null
    },
    {
     "name": "metadata",
     "type": "object",
     "ref": null
    },
    {
     "name": "chunk_count",
     "type": "integer",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "KnbDocumentsResponse",
   "type": "object",
   "required": [
    "documents"
   ],
   "properties": [
    {
     "name": "documents",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "KnbErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "KnbSearchRequest",
   "type": "object",
   "required": [
    "query"
   ],
   "properties": [
    {
     "name": "query",
     "type": "string",
     "ref": null
    },
    {
     "name": "k",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "KnbSearchResponse",
   "type": "object",
   "required": [
    "results"
   ],
   "properties": [
    {
     "name": "results",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "KnbSearchResult",
   "type": "object",
   "required": [
    "document_id",
    "chunk_ordinal",
    "content",
    "score",
    "citation"
   ],
   "properties": [
    {
     "name": "document_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "document_title",
     "type": "string",
     "ref": null
    },
    {
     "name": "chunk_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "chunk_ordinal",
     "type": "integer",
     "ref": null
    },
    {
     "name": "content",
     "type": "string",
     "ref": null
    },
    {
     "name": "score",
     "type": "number",
     "ref": null
    },
    {
     "name": "citation",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "LegacyTextError",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "ListRunsResponse",
   "type": "object",
   "required": [
    "runs"
   ],
   "properties": [
    {
     "name": "runs",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "ListSchedulesResponse",
   "type": "object",
   "required": [
    "schedules"
   ],
   "properties": [
    {
     "name": "schedules",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "LoginRequest",
   "type": "object",
   "required": [
    "email",
    "password"
   ],
   "properties": [
    {
     "name": "email",
     "type": "string",
     "ref": null
    },
    {
     "name": "password",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "LoginResponse",
   "type": "object",
   "required": [
    "token"
   ],
   "properties": [
    {
     "name": "token",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "McpCallToolParams",
   "type": "object",
   "required": [
    "name"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "arguments",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "McpCallToolResult",
   "type": "object",
   "required": [
    "content",
    "isError"
   ],
   "properties": [
    {
     "name": "content",
     "type": "array",
     "ref": null
    },
    {
     "name": "isError",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "McpInitializeParams",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "protocolVersion",
     "type": "string",
     "ref": null
    },
    {
     "name": "capabilities",
     "type": "object",
     "ref": null
    },
    {
     "name": "clientInfo",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "McpInitializeResult",
   "type": "object",
   "required": [
    "protocolVersion",
    "capabilities",
    "serverInfo"
   ],
   "properties": [
    {
     "name": "protocolVersion",
     "type": "string",
     "ref": null
    },
    {
     "name": "capabilities",
     "type": "McpServerCapabilities",
     "ref": "McpServerCapabilities"
    },
    {
     "name": "serverInfo",
     "type": "McpServerInfo",
     "ref": "McpServerInfo"
    }
   ]
  },
  {
   "name": "McpJSONRPCError",
   "type": "object",
   "required": [
    "code",
    "message"
   ],
   "properties": [
    {
     "name": "code",
     "type": "integer",
     "ref": null
    },
    {
     "name": "message",
     "type": "string",
     "ref": null
    },
    {
     "name": "data",
     "type": "-",
     "ref": null
    }
   ]
  },
  {
   "name": "McpJSONRPCRequest",
   "type": "object",
   "required": [
    "jsonrpc",
    "method"
   ],
   "properties": [
    {
     "name": "jsonrpc",
     "type": "string",
     "ref": null
    },
    {
     "name": "id",
     "type": "-",
     "ref": null
    },
    {
     "name": "method",
     "type": "string",
     "ref": null
    },
    {
     "name": "params",
     "type": "-",
     "ref": null
    }
   ]
  },
  {
   "name": "McpJSONRPCResponse",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "jsonrpc",
     "type": "string",
     "ref": null
    },
    {
     "name": "id",
     "type": "-",
     "ref": null
    },
    {
     "name": "result",
     "type": "-",
     "ref": null
    },
    {
     "name": "error",
     "type": "McpJSONRPCError",
     "ref": "McpJSONRPCError"
    }
   ]
  },
  {
   "name": "McpPingResult",
   "type": "object",
   "required": [],
   "properties": []
  },
  {
   "name": "McpRegDeletedEnvelope",
   "type": "object",
   "required": [
    "deleted",
    "id"
   ],
   "properties": [
    {
     "name": "deleted",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "McpRegError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "McpRegRemoteTool",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "input_schema",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "McpRegServer",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "endpoint_url",
     "type": "string",
     "ref": null
    },
    {
     "name": "secret_ref",
     "type": "string",
     "ref": null
    },
    {
     "name": "auth_header",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "last_check_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "protocol_version",
     "type": "string",
     "ref": null
    },
    {
     "name": "tools",
     "type": "array",
     "ref": null
    },
    {
     "name": "created_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "McpRegServerEnvelope",
   "type": "object",
   "required": [
    "mcp_server"
   ],
   "properties": [
    {
     "name": "mcp_server",
     "type": "McpRegServer",
     "ref": "McpRegServer"
    }
   ]
  },
  {
   "name": "McpRegServerInput",
   "type": "object",
   "required": [
    "name",
    "endpoint_url"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "endpoint_url",
     "type": "string",
     "ref": null
    },
    {
     "name": "secret_ref",
     "type": "string",
     "ref": null
    },
    {
     "name": "auth_header",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "McpRegServerListEnvelope",
   "type": "object",
   "required": [
    "mcp_servers"
   ],
   "properties": [
    {
     "name": "mcp_servers",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "McpServerCapabilities",
   "type": "object",
   "required": [
    "tools"
   ],
   "properties": [
    {
     "name": "tools",
     "type": "McpToolsCapability",
     "ref": "McpToolsCapability"
    }
   ]
  },
  {
   "name": "McpServerInfo",
   "type": "object",
   "required": [
    "name",
    "version"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "version",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "McpTextContent",
   "type": "object",
   "required": [
    "type",
    "text"
   ],
   "properties": [
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "text",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "McpToolDescriptor",
   "type": "object",
   "required": [
    "name"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "inputSchema",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "McpToolsCapability",
   "type": "object",
   "required": [
    "listChanged"
   ],
   "properties": [
    {
     "name": "listChanged",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "McpToolsListResult",
   "type": "object",
   "required": [
    "tools"
   ],
   "properties": [
    {
     "name": "tools",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "McpTrustDriftEvent",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "at",
     "type": "string",
     "ref": null
    },
    {
     "name": "old_hash",
     "type": "string",
     "ref": null
    },
    {
     "name": "new_hash",
     "type": "string",
     "ref": null
    },
    {
     "name": "added",
     "type": "array",
     "ref": null
    },
    {
     "name": "removed",
     "type": "array",
     "ref": null
    },
    {
     "name": "changed",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "McpTrustError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "McpTrustState",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "mcp_server",
     "type": "object",
     "ref": null
    },
    {
     "name": "toolset_hash",
     "type": "string",
     "ref": null
    },
    {
     "name": "pinned_toolset_hash",
     "type": "string",
     "ref": null
    },
    {
     "name": "last_drift_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "drift_history",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "MemErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "MemPutMemoryRequest",
   "type": "object",
   "required": [
    "snippets"
   ],
   "properties": [
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "snippets",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "MemSnippet",
   "type": "object",
   "required": [
    "id",
    "agent_id",
    "scope",
    "content",
    "importance",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "scope",
     "type": "string",
     "ref": null
    },
    {
     "name": "content",
     "type": "string",
     "ref": null
    },
    {
     "name": "importance",
     "type": "number",
     "ref": null
    },
    {
     "name": "expires_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "MemSnippetInput",
   "type": "object",
   "required": [
    "content"
   ],
   "properties": [
    {
     "name": "scope",
     "type": "string",
     "ref": null
    },
    {
     "name": "content",
     "type": "string",
     "ref": null
    },
    {
     "name": "importance",
     "type": "number",
     "ref": null
    },
    {
     "name": "expires_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "embedding",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "MemSnippetsResponse",
   "type": "object",
   "required": [
    "snippets"
   ],
   "properties": [
    {
     "name": "snippets",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "MessageSendRequest",
   "type": "object",
   "required": [
    "from_agent_id",
    "to_agent_id"
   ],
   "properties": [
    {
     "name": "from_agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "to_agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "message_type",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "payload",
     "type": "-",
     "ref": null
    }
   ]
  },
  {
   "name": "MetricsResponse",
   "type": "object",
   "required": [
    "counts",
    "latency",
    "queue_length"
   ],
   "properties": [
    {
     "name": "counts",
     "type": "object",
     "ref": null
    },
    {
     "name": "latency",
     "type": "object",
     "ref": null
    },
    {
     "name": "queue_length",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "MktError",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "MktInstallResponse",
   "type": "object",
   "required": [
    "listing",
    "agent"
   ],
   "properties": [
    {
     "name": "listing",
     "type": "MktListing",
     "ref": "MktListing"
    },
    {
     "name": "agent",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "MktListing",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "publisher_org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "publisher_user_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "source_agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "slug",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "tags",
     "type": "array",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "download_count",
     "type": "integer",
     "ref": null
    },
    {
     "name": "version_snapshot",
     "type": "-",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "MktListingEnvelope",
   "type": "object",
   "required": [
    "listing"
   ],
   "properties": [
    {
     "name": "listing",
     "type": "MktListing",
     "ref": "MktListing"
    }
   ]
  },
  {
   "name": "MktListingPage",
   "type": "object",
   "required": [
    "listings"
   ],
   "properties": [
    {
     "name": "listings",
     "type": "array",
     "ref": null
    },
    {
     "name": "next_cursor",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "MktPublishRequest",
   "type": "object",
   "required": [
    "agent_id",
    "name"
   ],
   "properties": [
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "tags",
     "type": "array",
     "ref": null
    },
    {
     "name": "signature",
     "type": "string",
     "ref": null
    },
    {
     "name": "signing_key_id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "MktSigError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "MktSigKey",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "public_key",
     "type": "string",
     "ref": null
    },
    {
     "name": "alg",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "fingerprint",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "revoked_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "MktSigKeyEnvelope",
   "type": "object",
   "required": [
    "key"
   ],
   "properties": [
    {
     "name": "key",
     "type": "MktSigKey",
     "ref": "MktSigKey"
    }
   ]
  },
  {
   "name": "MktSigKeyListEnvelope",
   "type": "object",
   "required": [
    "keys"
   ],
   "properties": [
    {
     "name": "keys",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "MktSigProvenance",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "slug",
     "type": "string",
     "ref": null
    },
    {
     "name": "publisher_org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "signing_key_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "fingerprint",
     "type": "string",
     "ref": null
    },
    {
     "name": "alg",
     "type": "string",
     "ref": null
    },
    {
     "name": "signed",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "signed_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "manifest_hash",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "MktSigProvenanceEnvelope",
   "type": "object",
   "required": [
    "provenance"
   ],
   "properties": [
    {
     "name": "provenance",
     "type": "MktSigProvenance",
     "ref": "MktSigProvenance"
    }
   ]
  },
  {
   "name": "MktSigRegisterRequest",
   "type": "object",
   "required": [
    "public_key"
   ],
   "properties": [
    {
     "name": "public_key",
     "type": "string",
     "ref": null
    },
    {
     "name": "alg",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "MktSigRevokedEnvelope",
   "type": "object",
   "required": [
    "revoked"
   ],
   "properties": [
    {
     "name": "revoked",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "MktSigSettings",
   "type": "object",
   "required": [
    "allow_unsigned"
   ],
   "properties": [
    {
     "name": "allow_unsigned",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "MktSigSettingsEnvelope",
   "type": "object",
   "required": [
    "settings"
   ],
   "properties": [
    {
     "name": "settings",
     "type": "MktSigSettings",
     "ref": "MktSigSettings"
    }
   ]
  },
  {
   "name": "MktSigSettingsUpdate",
   "type": "object",
   "required": [
    "allow_unsigned"
   ],
   "properties": [
    {
     "name": "allow_unsigned",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "ModelCapabilities",
   "type": "object",
   "required": [
    "model"
   ],
   "properties": [
    {
     "name": "model",
     "type": "string",
     "ref": null
    },
    {
     "name": "context_tokens",
     "type": "integer",
     "ref": null
    },
    {
     "name": "tool_support",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "vision_support",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "quality_tier",
     "type": "string",
     "ref": null
    },
    {
     "name": "latency_estimate_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "input_cost_per_1k",
     "type": "number",
     "ref": null
    },
    {
     "name": "output_cost_per_1k",
     "type": "number",
     "ref": null
    }
   ]
  },
  {
   "name": "NcDLQEntry",
   "type": "object",
   "required": [
    "sequence",
    "subject",
    "error",
    "attempts",
    "dead_lettered_at"
   ],
   "properties": [
    {
     "name": "sequence",
     "type": "integer",
     "ref": null
    },
    {
     "name": "subject",
     "type": "string",
     "ref": null
    },
    {
     "name": "task_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "error",
     "type": "string",
     "ref": null
    },
    {
     "name": "attempts",
     "type": "integer",
     "ref": null
    },
    {
     "name": "dead_lettered_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "payload_base64",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "NcErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "NcMessagingMode",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "NcNodeCompletedEnvelope",
   "type": "object",
   "required": [
    "organization_id",
    "workflow_run_id",
    "node_id",
    "status",
    "completed_at"
   ],
   "properties": [
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "workflow_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "node_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "output",
     "type": "string",
     "ref": null
    },
    {
     "name": "error",
     "type": "string",
     "ref": null
    },
    {
     "name": "completed_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "NcReplayRequest",
   "type": "object",
   "required": [
    "subject",
    "from"
   ],
   "properties": [
    {
     "name": "subject",
     "type": "string",
     "ref": null
    },
    {
     "name": "from",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "NcReplayResult",
   "type": "object",
   "required": [
    "subject",
    "from",
    "replayed"
   ],
   "properties": [
    {
     "name": "subject",
     "type": "string",
     "ref": null
    },
    {
     "name": "from",
     "type": "string",
     "ref": null
    },
    {
     "name": "replayed",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "NcRunEnvelope",
   "type": "object",
   "required": [
    "task_id",
    "task_type",
    "created_at"
   ],
   "properties": [
    {
     "name": "task_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "task_type",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "trace_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "payload",
     "type": "object",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "NodeRun",
   "type": "object",
   "required": [
    "id",
    "workflow_run_id",
    "node_id",
    "status",
    "created_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "workflow_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "node_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "NodeRunStatus",
     "ref": "NodeRunStatus"
    },
    {
     "name": "error",
     "type": "string",
     "ref": null
    },
    {
     "name": "started_at",
     "type": "['string', 'null']",
     "ref": null
    },
    {
     "name": "finished_at",
     "type": "['string', 'null']",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "NodeRunStatus",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "NsubCreateSubscriptionRequest",
   "type": "object",
   "required": [
    "webhook_id",
    "event_types"
   ],
   "properties": [
    {
     "name": "webhook_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "event_types",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "NsubCreateSubscriptionResponse",
   "type": "object",
   "required": [
    "subscription"
   ],
   "properties": [
    {
     "name": "subscription",
     "type": "NsubSubscription",
     "ref": "NsubSubscription"
    }
   ]
  },
  {
   "name": "NsubDeleteResponse",
   "type": "object",
   "required": [
    "deleted"
   ],
   "properties": [
    {
     "name": "deleted",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "NsubError",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "NsubSubscription",
   "type": "object",
   "required": [
    "id",
    "webhook_id",
    "event_types",
    "created_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "webhook_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "event_types",
     "type": "array",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "NsubSubscriptionList",
   "type": "object",
   "required": [
    "subscriptions"
   ],
   "properties": [
    {
     "name": "subscriptions",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "OrchestrationError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "OrchestrationUsage",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "prompt_tokens",
     "type": "integer",
     "ref": null
    },
    {
     "name": "completion_tokens",
     "type": "integer",
     "ref": null
    },
    {
     "name": "total_tokens",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "Organization",
   "type": "object",
   "required": [
    "ID",
    "Name"
   ],
   "properties": [
    {
     "name": "ID",
     "type": "string",
     "ref": null
    },
    {
     "name": "Name",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "OrgErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "OrgInfoResponse",
   "type": "object",
   "required": [
    "organization",
    "role"
   ],
   "properties": [
    {
     "name": "organization",
     "type": "OrgSummary",
     "ref": "OrgSummary"
    },
    {
     "name": "role",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "OrgMember",
   "type": "object",
   "required": [
    "id",
    "email",
    "name",
    "role",
    "joined_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "email",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "role",
     "type": "string",
     "ref": null
    },
    {
     "name": "joined_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "OrgMemberAddRequest",
   "type": "object",
   "required": [
    "email",
    "role"
   ],
   "properties": [
    {
     "name": "email",
     "type": "string",
     "ref": null
    },
    {
     "name": "role",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "OrgMemberListResponse",
   "type": "object",
   "required": [
    "members"
   ],
   "properties": [
    {
     "name": "members",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "OrgMemberMutationResponse",
   "type": "object",
   "required": [
    "member"
   ],
   "properties": [
    {
     "name": "member",
     "type": "OrgMember",
     "ref": "OrgMember"
    }
   ]
  },
  {
   "name": "OrgMemberRemoveResponse",
   "type": "object",
   "required": [
    "removed"
   ],
   "properties": [
    {
     "name": "removed",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "OrgMemberRoleUpdateRequest",
   "type": "object",
   "required": [
    "role"
   ],
   "properties": [
    {
     "name": "role",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "OrgSummary",
   "type": "object",
   "required": [
    "id",
    "name",
    "status",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "PolEnfErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "PolEnfWaitingApprovalResponse",
   "type": "object",
   "required": [
    "run_id",
    "status",
    "policy"
   ],
   "properties": [
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "approval_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "policy",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "Policy",
   "type": "object",
   "required": [
    "name",
    "effect",
    "resource_type",
    "actions",
    "conditions",
    "priority",
    "enabled",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "effect",
     "type": "string",
     "ref": null
    },
    {
     "name": "resource_type",
     "type": "string",
     "ref": null
    },
    {
     "name": "actions",
     "type": "array",
     "ref": null
    },
    {
     "name": "conditions",
     "type": "PolicyConditions",
     "ref": "PolicyConditions"
    },
    {
     "name": "priority",
     "type": "integer",
     "ref": null
    },
    {
     "name": "enabled",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "PolicyConditions",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "tool_allowlist",
     "type": "array",
     "ref": null
    },
    {
     "name": "max_cost_cents",
     "type": "-",
     "ref": null
    },
    {
     "name": "environments",
     "type": "array",
     "ref": null
    },
    {
     "name": "require_approval",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "PolicyEvaluateRequest",
   "type": "object",
   "required": [
    "action",
    "resource",
    "context"
   ],
   "properties": [
    {
     "name": "subject",
     "type": "object",
     "ref": null
    },
    {
     "name": "action",
     "type": "string",
     "ref": null
    },
    {
     "name": "resource",
     "type": "object",
     "ref": null
    },
    {
     "name": "context",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "PolicyEvaluateResponse",
   "type": "object",
   "required": [
    "decision",
    "matched_policy_id",
    "reason"
   ],
   "properties": [
    {
     "name": "decision",
     "type": "string",
     "ref": null
    },
    {
     "name": "matched_policy_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "reason",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "PolicyListResponse",
   "type": "object",
   "required": [
    "policies"
   ],
   "properties": [
    {
     "name": "policies",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "PolicyResponse",
   "type": "object",
   "required": [
    "policy"
   ],
   "properties": [
    {
     "name": "policy",
     "type": "Policy",
     "ref": "Policy"
    }
   ]
  },
  {
   "name": "PortAgent",
   "type": "object",
   "required": [
    "ID",
    "OrganizationID",
    "Name",
    "Model",
    "Status",
    "CreatedAt",
    "UpdatedAt"
   ],
   "properties": [
    {
     "name": "ID",
     "type": "string",
     "ref": null
    },
    {
     "name": "OrganizationID",
     "type": "string",
     "ref": null
    },
    {
     "name": "Name",
     "type": "string",
     "ref": null
    },
    {
     "name": "Description",
     "type": "string",
     "ref": null
    },
    {
     "name": "Instructions",
     "type": "string",
     "ref": null
    },
    {
     "name": "Model",
     "type": "string",
     "ref": null
    },
    {
     "name": "Status",
     "type": "string",
     "ref": null
    },
    {
     "name": "CurrentVersionID",
     "type": "string",
     "ref": null
    },
    {
     "name": "CreatedAt",
     "type": "string",
     "ref": null
    },
    {
     "name": "UpdatedAt",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "PortErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "PublishEventRequest",
   "type": "object",
   "required": [
    "type",
    "name",
    "payload"
   ],
   "properties": [
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "payload",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "QtErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "QtStatsResponse",
   "type": "object",
   "required": [
    "stats",
    "total"
   ],
   "properties": [
    {
     "name": "stats",
     "type": "object",
     "ref": null
    },
    {
     "name": "total",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "QtTask",
   "type": "object",
   "required": [
    "id",
    "type",
    "status",
    "attempts",
    "requeues",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "attempts",
     "type": "integer",
     "ref": null
    },
    {
     "name": "requeues",
     "type": "integer",
     "ref": null
    },
    {
     "name": "last_error",
     "type": "string",
     "ref": null
    },
    {
     "name": "payload",
     "type": "object",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "requeued_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "QtTaskListResponse",
   "type": "object",
   "required": [
    "tasks",
    "next_cursor"
   ],
   "properties": [
    {
     "name": "tasks",
     "type": "array",
     "ref": null
    },
    {
     "name": "next_cursor",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "QualityGateCheckResult",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "suite",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "QualityGateCheckStatus",
     "ref": "QualityGateCheckStatus"
    },
    {
     "name": "reason",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "QualityGateCheckStatus",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "QualityGateDecision",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "policy_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "policy_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "policy_evaluation_version",
     "type": "string",
     "ref": null
    },
    {
     "name": "recommendation",
     "type": "QualityGateRecommendation",
     "ref": "QualityGateRecommendation"
    },
    {
     "name": "check_results",
     "type": "array",
     "ref": null
    },
    {
     "name": "evaluated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "QualityGateEvidenceFinding",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "severity",
     "type": "QualityGateSeverity",
     "ref": "QualityGateSeverity"
    },
    {
     "name": "title",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "QualityGateEvidenceRecord",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "suite",
     "type": "QualityGateSuiteKind",
     "ref": "QualityGateSuiteKind"
    },
    {
     "name": "subject",
     "type": "QualityGateEvidenceSubject",
     "ref": "QualityGateEvidenceSubject"
    },
    {
     "name": "result",
     "type": "QualityGateSuiteResult",
     "ref": "QualityGateSuiteResult"
    },
    {
     "name": "collected_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "content_hash",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "QualityGateEvidenceStatus",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "QualityGateEvidenceSubject",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "app_version",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "model_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "prompt_version",
     "type": "string",
     "ref": null
    },
    {
     "name": "tool_versions",
     "type": "array",
     "ref": null
    },
    {
     "name": "workflow_version",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "QualityGateException",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "suite",
     "type": "-",
     "ref": null
    },
    {
     "name": "reason",
     "type": "string",
     "ref": null
    },
    {
     "name": "approved_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "approved_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "expires_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "QualityGatePolicy",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "required_suites",
     "type": "array",
     "ref": null
    },
    {
     "name": "exceptions",
     "type": "array",
     "ref": null
    },
    {
     "name": "active",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "created_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "QualityGatePolicyInput",
   "type": "object",
   "required": [
    "name",
    "required_suites"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "required_suites",
     "type": "array",
     "ref": null
    },
    {
     "name": "active",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "QualityGateRecommendation",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "QualityGatesError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "QualityGateSeverity",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "QualityGateSuiteKind",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "QualityGateSuiteRequirement",
   "type": "object",
   "required": [
    "kind"
   ],
   "properties": [
    {
     "name": "kind",
     "type": "QualityGateSuiteKind",
     "ref": "QualityGateSuiteKind"
    },
    {
     "name": "min_coverage_pct",
     "type": "number",
     "ref": null
    },
    {
     "name": "min_pass_rate",
     "type": "number",
     "ref": null
    },
    {
     "name": "max_p95_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "max_severity_allowed",
     "type": "-",
     "ref": null
    },
    {
     "name": "max_age_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "fail_closed",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "QualityGateSuiteResult",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "status",
     "type": "QualityGateEvidenceStatus",
     "ref": "QualityGateEvidenceStatus"
    },
    {
     "name": "coverage_pct",
     "type": "number",
     "ref": null
    },
    {
     "name": "cases",
     "type": "integer",
     "ref": null
    },
    {
     "name": "passed",
     "type": "integer",
     "ref": null
    },
    {
     "name": "p95_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "findings",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "QuotaErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "RateLimit",
   "type": "object",
   "required": [
    "scope",
    "subject_id",
    "rpm",
    "rpd",
    "tpm",
    "enabled"
   ],
   "properties": [
    {
     "name": "scope",
     "type": "string",
     "ref": null
    },
    {
     "name": "subject_id",
     "type": "['string', 'null']",
     "ref": null
    },
    {
     "name": "rpm",
     "type": "integer",
     "ref": null
    },
    {
     "name": "rpd",
     "type": "integer",
     "ref": null
    },
    {
     "name": "tpm",
     "type": "integer",
     "ref": null
    },
    {
     "name": "enabled",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "org_default",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "RateLimitDeleteResponse",
   "type": "object",
   "required": [
    "deleted",
    "scope",
    "subject_id"
   ],
   "properties": [
    {
     "name": "deleted",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "scope",
     "type": "string",
     "ref": null
    },
    {
     "name": "subject_id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "RateLimitedError",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "RateLimitErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "RateLimitListResponse",
   "type": "object",
   "required": [
    "limits"
   ],
   "properties": [
    {
     "name": "limits",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "RateLimitUpsertRequest",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "rpm",
     "type": "integer",
     "ref": null
    },
    {
     "name": "rpd",
     "type": "integer",
     "ref": null
    },
    {
     "name": "tpm",
     "type": "integer",
     "ref": null
    },
    {
     "name": "enabled",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "RateLimitUpsertResponse",
   "type": "object",
   "required": [
    "limit"
   ],
   "properties": [
    {
     "name": "limit",
     "type": "RateLimit",
     "ref": "RateLimit"
    }
   ]
  },
  {
   "name": "RegisterRequest",
   "type": "object",
   "required": [
    "organization",
    "email",
    "password"
   ],
   "properties": [
    {
     "name": "organization",
     "type": "string",
     "ref": null
    },
    {
     "name": "email",
     "type": "string",
     "ref": null
    },
    {
     "name": "password",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "RegisterResponse",
   "type": "object",
   "required": [
    "organization",
    "user"
   ],
   "properties": [
    {
     "name": "organization",
     "type": "Organization",
     "ref": "Organization"
    },
    {
     "name": "user",
     "type": "User",
     "ref": "User"
    }
   ]
  },
  {
   "name": "ResidencyError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ResidencyMode",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "ResidencyPolicy",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "pinned_region",
     "type": "string",
     "ref": null
    },
    {
     "name": "allowed_regions",
     "type": "array",
     "ref": null
    },
    {
     "name": "strict_mode",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ResidencyPolicyInput",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "pinned_region",
     "type": "string",
     "ref": null
    },
    {
     "name": "allowed_regions",
     "type": "array",
     "ref": null
    },
    {
     "name": "strict_mode",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "ResidencyPosture",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "pinned_region",
     "type": "string",
     "ref": null
    },
    {
     "name": "allowed_regions",
     "type": "array",
     "ref": null
    },
    {
     "name": "strict_mode",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "mode",
     "type": "ResidencyMode",
     "ref": "ResidencyMode"
    },
    {
     "name": "last_violation",
     "type": "-",
     "ref": null
    }
   ]
  },
  {
   "name": "ResidencyRegion",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "ResidencyVerdict",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "allowed",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "region",
     "type": "ResidencyRegion",
     "ref": "ResidencyRegion"
    },
    {
     "name": "reason",
     "type": "string",
     "ref": null
    },
    {
     "name": "strict_mode",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "mode",
     "type": "ResidencyMode",
     "ref": "ResidencyMode"
    }
   ]
  },
  {
   "name": "ResidencyViolationError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ResidencyViolationEvidence",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "at",
     "type": "string",
     "ref": null
    },
    {
     "name": "url_host",
     "type": "string",
     "ref": null
    },
    {
     "name": "region",
     "type": "ResidencyRegion",
     "ref": "ResidencyRegion"
    },
    {
     "name": "reason",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "RollbackAgentRequest",
   "type": "object",
   "required": [
    "target_version"
   ],
   "properties": [
    {
     "name": "target_version",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "RollbackDeploymentResponse",
   "type": "object",
   "required": [
    "deployment",
    "rolled_back_to_version"
   ],
   "properties": [
    {
     "name": "deployment",
     "type": "Deployment",
     "ref": "Deployment"
    },
    {
     "name": "rolled_back_to_version",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "RouterDecision",
   "type": "object",
   "required": [
    "policy",
    "complexity"
   ],
   "properties": [
    {
     "name": "policy",
     "type": "RoutingPolicy",
     "ref": "RoutingPolicy"
    },
    {
     "name": "complexity",
     "type": "string",
     "ref": null
    },
    {
     "name": "selected_model",
     "type": "string",
     "ref": null
    },
    {
     "name": "provider_name",
     "type": "string",
     "ref": null
    },
    {
     "name": "reason",
     "type": "string",
     "ref": null
    },
    {
     "name": "candidate_order",
     "type": "array",
     "ref": null
    },
    {
     "name": "skipped",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "RoutingPolicy",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "Run",
   "type": "object",
   "required": [
    "ID",
    "OrganizationID",
    "AgentID",
    "Status",
    "CreatedAt",
    "UpdatedAt"
   ],
   "properties": [
    {
     "name": "ID",
     "type": "string",
     "ref": null
    },
    {
     "name": "OrganizationID",
     "type": "string",
     "ref": null
    },
    {
     "name": "AgentID",
     "type": "string",
     "ref": null
    },
    {
     "name": "Input",
     "type": "string",
     "ref": null
    },
    {
     "name": "Output",
     "type": "string",
     "ref": null
    },
    {
     "name": "Status",
     "type": "string",
     "ref": null
    },
    {
     "name": "CreatedAt",
     "type": "string",
     "ref": null
    },
    {
     "name": "UpdatedAt",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "RunControlResponse",
   "type": "object",
   "required": [
    "ID",
    "Status"
   ],
   "properties": [
    {
     "name": "ID",
     "type": "string",
     "ref": null
    },
    {
     "name": "OrganizationID",
     "type": "string",
     "ref": null
    },
    {
     "name": "AgentID",
     "type": "string",
     "ref": null
    },
    {
     "name": "Input",
     "type": "string",
     "ref": null
    },
    {
     "name": "Output",
     "type": "string",
     "ref": null
    },
    {
     "name": "Status",
     "type": "string",
     "ref": null
    },
    {
     "name": "CreatedAt",
     "type": "string",
     "ref": null
    },
    {
     "name": "UpdatedAt",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "RunEvent",
   "type": "object",
   "required": [
    "run_id",
    "type",
    "name",
    "payload",
    "created_at"
   ],
   "properties": [
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "payload",
     "type": "object",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "RunEventsResponse",
   "type": "object",
   "required": [
    "run_id",
    "events"
   ],
   "properties": [
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "events",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "RunLifecycleErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "RunLifecycleRun",
   "type": "object",
   "required": [
    "id",
    "organization_id",
    "agent_id",
    "status",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "RunLifecycleState",
     "ref": "RunLifecycleState"
    },
    {
     "name": "input",
     "type": "string",
     "ref": null
    },
    {
     "name": "output",
     "type": "string",
     "ref": null
    },
    {
     "name": "cost_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "RunLifecycleRunResponse",
   "type": "object",
   "required": [
    "id",
    "organization_id",
    "agent_id",
    "status",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "organization_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "RunLifecycleState",
     "ref": "RunLifecycleState"
    },
    {
     "name": "input",
     "type": "string",
     "ref": null
    },
    {
     "name": "output",
     "type": "string",
     "ref": null
    },
    {
     "name": "cost_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "RunLifecycleState",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "RunLifecycleTransitionError",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "RunStatus",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "RunStep",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "ID",
     "type": "string",
     "ref": null
    },
    {
     "name": "RunID",
     "type": "string",
     "ref": null
    },
    {
     "name": "StepType",
     "type": "string",
     "ref": null
    },
    {
     "name": "Status",
     "type": "string",
     "ref": null
    },
    {
     "name": "InputMeta",
     "type": "object",
     "ref": null
    },
    {
     "name": "OutputMeta",
     "type": "object",
     "ref": null
    },
    {
     "name": "Error",
     "type": "string",
     "ref": null
    },
    {
     "name": "TokenUsage",
     "type": "object",
     "ref": null
    },
    {
     "name": "Cost",
     "type": "number",
     "ref": null
    },
    {
     "name": "StartedAt",
     "type": "string",
     "ref": null
    },
    {
     "name": "CompletedAt",
     "type": "string",
     "ref": null
    },
    {
     "name": "CreatedAt",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "RunStepsResponse",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "steps",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "RuntimeBudgetSnapshot",
   "type": "object",
   "required": [
    "phase",
    "prompt_tokens",
    "completion_tokens",
    "total_tokens"
   ],
   "properties": [
    {
     "name": "phase",
     "type": "string",
     "ref": null
    },
    {
     "name": "prompt_tokens",
     "type": "integer",
     "ref": null
    },
    {
     "name": "completion_tokens",
     "type": "integer",
     "ref": null
    },
    {
     "name": "total_tokens",
     "type": "integer",
     "ref": null
    },
    {
     "name": "max_total_tokens",
     "type": "integer",
     "ref": null
    },
    {
     "name": "max_prompt_tokens",
     "type": "integer",
     "ref": null
    },
    {
     "name": "max_completion_tokens",
     "type": "integer",
     "ref": null
    },
    {
     "name": "alert_threshold",
     "type": "number",
     "ref": null
    },
    {
     "name": "limit",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "RuntimeConfigDocument",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "conversation_history",
     "type": "RuntimeConversationHistoryConfig",
     "ref": "RuntimeConversationHistoryConfig"
    },
    {
     "name": "token_budget",
     "type": "RuntimeTokenBudgetConfig",
     "ref": "RuntimeTokenBudgetConfig"
    }
   ]
  },
  {
   "name": "RuntimeConversationHistoryConfig",
   "type": "object",
   "required": [
    "strategy",
    "max_tokens"
   ],
   "properties": [
    {
     "name": "strategy",
     "type": "string",
     "ref": null
    },
    {
     "name": "max_tokens",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "RuntimeTokenBudgetConfig",
   "type": "object",
   "required": [
    "max_total_tokens",
    "max_prompt_tokens",
    "max_completion_tokens",
    "alert_threshold"
   ],
   "properties": [
    {
     "name": "max_total_tokens",
     "type": "integer",
     "ref": null
    },
    {
     "name": "max_prompt_tokens",
     "type": "integer",
     "ref": null
    },
    {
     "name": "max_completion_tokens",
     "type": "integer",
     "ref": null
    },
    {
     "name": "alert_threshold",
     "type": "number",
     "ref": null
    }
   ]
  },
  {
   "name": "Schedule",
   "type": "object",
   "required": [
    "id",
    "agent_id",
    "kind",
    "status",
    "timezone",
    "created_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "input",
     "type": "string",
     "ref": null
    },
    {
     "name": "kind",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_at",
     "type": "['string', 'null']",
     "ref": null
    },
    {
     "name": "interval_seconds",
     "type": "integer",
     "ref": null
    },
    {
     "name": "cron_expr",
     "type": "string",
     "ref": null
    },
    {
     "name": "timezone",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "next_run_at",
     "type": "['string', 'null']",
     "ref": null
    },
    {
     "name": "last_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "last_fired_at",
     "type": "['string', 'null']",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ScheduleCreateRequest",
   "type": "object",
   "required": [
    "agent_id",
    "kind"
   ],
   "properties": [
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "input",
     "type": "string",
     "ref": null
    },
    {
     "name": "kind",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "interval_seconds",
     "type": "integer",
     "ref": null
    },
    {
     "name": "cron_expr",
     "type": "string",
     "ref": null
    },
    {
     "name": "timezone",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ScheduleResponse",
   "type": "object",
   "required": [
    "schedule"
   ],
   "properties": [
    {
     "name": "schedule",
     "type": "Schedule",
     "ref": "Schedule"
    }
   ]
  },
  {
   "name": "ScimError",
   "type": "object",
   "required": [
    "schemas",
    "status",
    "detail"
   ],
   "properties": [
    {
     "name": "schemas",
     "type": "array",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "detail",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ScimListResponse",
   "type": "object",
   "required": [
    "schemas",
    "totalResults",
    "startIndex",
    "itemsPerPage",
    "Resources"
   ],
   "properties": [
    {
     "name": "schemas",
     "type": "array",
     "ref": null
    },
    {
     "name": "totalResults",
     "type": "integer",
     "ref": null
    },
    {
     "name": "startIndex",
     "type": "integer",
     "ref": null
    },
    {
     "name": "itemsPerPage",
     "type": "integer",
     "ref": null
    },
    {
     "name": "Resources",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "ScimPatchOperation",
   "type": "object",
   "required": [
    "op"
   ],
   "properties": [
    {
     "name": "op",
     "type": "string",
     "ref": null
    },
    {
     "name": "path",
     "type": "string",
     "ref": null
    },
    {
     "name": "value",
     "type": "-",
     "ref": null
    }
   ]
  },
  {
   "name": "ScimPatchRequest",
   "type": "object",
   "required": [
    "Operations"
   ],
   "properties": [
    {
     "name": "schemas",
     "type": "array",
     "ref": null
    },
    {
     "name": "Operations",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "ScimResourceMeta",
   "type": "object",
   "required": [
    "resourceType",
    "created",
    "lastModified",
    "location"
   ],
   "properties": [
    {
     "name": "resourceType",
     "type": "string",
     "ref": null
    },
    {
     "name": "created",
     "type": "string",
     "ref": null
    },
    {
     "name": "lastModified",
     "type": "string",
     "ref": null
    },
    {
     "name": "location",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ScimTokenCreated",
   "type": "object",
   "required": [
    "token",
    "secret"
   ],
   "properties": [
    {
     "name": "token",
     "type": "object",
     "ref": null
    },
    {
     "name": "secret",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ScimTokenRevokeErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ScimUser",
   "type": "object",
   "required": [
    "schemas",
    "id",
    "userName",
    "active",
    "meta"
   ],
   "properties": [
    {
     "name": "schemas",
     "type": "array",
     "ref": null
    },
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "userName",
     "type": "string",
     "ref": null
    },
    {
     "name": "active",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "emails",
     "type": "array",
     "ref": null
    },
    {
     "name": "meta",
     "type": "ScimResourceMeta",
     "ref": "ScimResourceMeta"
    }
   ]
  },
  {
   "name": "ScimUserEmail",
   "type": "object",
   "required": [
    "value",
    "primary"
   ],
   "properties": [
    {
     "name": "value",
     "type": "string",
     "ref": null
    },
    {
     "name": "primary",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "ScimUserRequest",
   "type": "object",
   "required": [
    "userName"
   ],
   "properties": [
    {
     "name": "schemas",
     "type": "array",
     "ref": null
    },
    {
     "name": "userName",
     "type": "string",
     "ref": null
    },
    {
     "name": "active",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "password",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "object",
     "ref": null
    },
    {
     "name": "displayName",
     "type": "string",
     "ref": null
    },
    {
     "name": "externalId",
     "type": "string",
     "ref": null
    },
    {
     "name": "emails",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "SearchInput",
   "type": "object",
   "required": [
    "query"
   ],
   "properties": [
    {
     "name": "query",
     "type": "string",
     "ref": null
    },
    {
     "name": "mode",
     "type": "string",
     "ref": null
    },
    {
     "name": "k",
     "type": "integer",
     "ref": null
    },
    {
     "name": "timeout_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "estimated_cost_cents",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "SearchOutput",
   "type": "object",
   "required": [
    "mode",
    "query",
    "results",
    "count",
    "warnings"
   ],
   "properties": [
    {
     "name": "mode",
     "type": "string",
     "ref": null
    },
    {
     "name": "query",
     "type": "string",
     "ref": null
    },
    {
     "name": "results",
     "type": "array",
     "ref": null
    },
    {
     "name": "count",
     "type": "integer",
     "ref": null
    },
    {
     "name": "warnings",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "SearchResult",
   "type": "object",
   "required": [
    "title",
    "snippet",
    "score",
    "source"
   ],
   "properties": [
    {
     "name": "title",
     "type": "string",
     "ref": null
    },
    {
     "name": "url",
     "type": "string",
     "ref": null
    },
    {
     "name": "snippet",
     "type": "string",
     "ref": null
    },
    {
     "name": "score",
     "type": "number",
     "ref": null
    },
    {
     "name": "source",
     "type": "string",
     "ref": null
    },
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "document_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "chunk_ordinal",
     "type": "integer",
     "ref": null
    },
    {
     "name": "citation",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SecCreateRequest",
   "type": "object",
   "required": [
    "name",
    "value"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "value",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SecDeleteResponse",
   "type": "object",
   "required": [
    "deleted"
   ],
   "properties": [
    {
     "name": "deleted",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "SecErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "SecListResponse",
   "type": "object",
   "required": [
    "secrets"
   ],
   "properties": [
    {
     "name": "secrets",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "SecMetadata",
   "type": "object",
   "required": [
    "name",
    "key_version",
    "created_by",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "key_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "created_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SecResponse",
   "type": "object",
   "required": [
    "secret"
   ],
   "properties": [
    {
     "name": "secret",
     "type": "SecMetadata",
     "ref": "SecMetadata"
    }
   ]
  },
  {
   "name": "SecRevealMetadata",
   "type": "object",
   "required": [
    "name",
    "key_version",
    "created_by",
    "created_at",
    "updated_at",
    "value"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "key_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "created_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "value",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SecRevealResponse",
   "type": "object",
   "required": [
    "secret"
   ],
   "properties": [
    {
     "name": "secret",
     "type": "SecRevealMetadata",
     "ref": "SecRevealMetadata"
    }
   ]
  },
  {
   "name": "ShowbackError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ShowbackReport",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "from",
     "type": "string",
     "ref": null
    },
    {
     "name": "to",
     "type": "string",
     "ref": null
    },
    {
     "name": "group_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "rows",
     "type": "array",
     "ref": null
    },
    {
     "name": "notes",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "ShowbackRow",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "scope_ref",
     "type": "string",
     "ref": null
    },
    {
     "name": "runs",
     "type": "integer",
     "ref": null
    },
    {
     "name": "tool_calls",
     "type": "integer",
     "ref": null
    },
    {
     "name": "tokens_in",
     "type": "integer",
     "ref": null
    },
    {
     "name": "tokens_out",
     "type": "integer",
     "ref": null
    },
    {
     "name": "cost_cents",
     "type": "number",
     "ref": null
    }
   ]
  },
  {
   "name": "SmErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "SmMarginPlan",
   "type": "object",
   "required": [
    "id",
    "name",
    "price_cents",
    "currency",
    "included_quota",
    "unlimited"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "price_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "currency",
     "type": "string",
     "ref": null
    },
    {
     "name": "included_quota",
     "type": "integer",
     "ref": null
    },
    {
     "name": "unlimited",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "SmMarginReport",
   "type": "object",
   "required": [
    "subscription_id",
    "status",
    "period_start",
    "period_end",
    "currency",
    "price_cents",
    "usage_cost_cents",
    "margin_cents",
    "plan"
   ],
   "properties": [
    {
     "name": "subscription_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "period_start",
     "type": "string",
     "ref": null
    },
    {
     "name": "period_end",
     "type": "string",
     "ref": null
    },
    {
     "name": "currency",
     "type": "string",
     "ref": null
    },
    {
     "name": "price_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "usage_cost_cents",
     "type": "number",
     "ref": null
    },
    {
     "name": "margin_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "margin_percent",
     "type": "number",
     "ref": null
    },
    {
     "name": "plan",
     "type": "SmMarginPlan",
     "ref": "SmMarginPlan"
    }
   ]
  },
  {
   "name": "SmMarginResponse",
   "type": "object",
   "required": [
    "margin"
   ],
   "properties": [
    {
     "name": "margin",
     "type": "SmMarginReport",
     "ref": "SmMarginReport"
    }
   ]
  },
  {
   "name": "SmMeters",
   "type": "object",
   "required": [
    "runs_count",
    "tool_calls_count"
   ],
   "properties": [
    {
     "name": "runs_count",
     "type": "integer",
     "ref": null
    },
    {
     "name": "tool_calls_count",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "SmMetersResponse",
   "type": "object",
   "required": [
    "from",
    "to",
    "meters"
   ],
   "properties": [
    {
     "name": "from",
     "type": "string",
     "ref": null
    },
    {
     "name": "to",
     "type": "string",
     "ref": null
    },
    {
     "name": "meters",
     "type": "SmMeters",
     "ref": "SmMeters"
    }
   ]
  },
  {
   "name": "SsoError",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "SsoLoginResponse",
   "type": "object",
   "required": [
    "token",
    "user"
   ],
   "properties": [
    {
     "name": "token",
     "type": "string",
     "ref": null
    },
    {
     "name": "user",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "SsoUnauthorized",
   "type": "-",
   "required": [],
   "properties": []
  },
  {
   "name": "SupervisorConfig",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "workers",
     "type": "array",
     "ref": null
    },
    {
     "name": "strategy",
     "type": "string",
     "ref": null
    },
    {
     "name": "max_concurrent",
     "type": "integer",
     "ref": null
    },
    {
     "name": "max_retries",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "SupervisorFailedError",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    },
    {
     "name": "task_ref",
     "type": "string",
     "ref": null
    },
    {
     "name": "attempts",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "SupervisorRunRequest",
   "type": "object",
   "required": [
    "task"
   ],
   "properties": [
    {
     "name": "task",
     "type": "string",
     "ref": null
    },
    {
     "name": "task_ref",
     "type": "string",
     "ref": null
    },
    {
     "name": "from_agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "parent_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "required_capability",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SupervisorRunResult",
   "type": "object",
   "required": [
    "task",
    "status",
    "worker_agent_id",
    "delegation_id",
    "child_run_id",
    "tokens",
    "attempts"
   ],
   "properties": [
    {
     "name": "task_ref",
     "type": "string",
     "ref": null
    },
    {
     "name": "task",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "worker_agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "delegation_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "child_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "output",
     "type": "string",
     "ref": null
    },
    {
     "name": "tokens",
     "type": "OrchestrationUsage",
     "ref": "OrchestrationUsage"
    },
    {
     "name": "attempts",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthAfterDeployment",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "app_version",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthAssertion",
   "type": "object",
   "required": [
    "kind",
    "target",
    "expected",
    "severity"
   ],
   "properties": [
    {
     "name": "kind",
     "type": "string",
     "ref": null
    },
    {
     "name": "target",
     "type": "string",
     "ref": null
    },
    {
     "name": "expected",
     "type": "string",
     "ref": null
    },
    {
     "name": "severity",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthAssertionOutcome",
   "type": "object",
   "required": [
    "index",
    "kind",
    "passed"
   ],
   "properties": [
    {
     "name": "index",
     "type": "integer",
     "ref": null
    },
    {
     "name": "kind",
     "type": "string",
     "ref": null
    },
    {
     "name": "target",
     "type": "string",
     "ref": null
    },
    {
     "name": "expected",
     "type": "string",
     "ref": null
    },
    {
     "name": "actual",
     "type": "string",
     "ref": null
    },
    {
     "name": "passed",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "message",
     "type": "string",
     "ref": null
    },
    {
     "name": "severity",
     "type": "string",
     "ref": null
    },
    {
     "name": "at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthCandidateJourney",
   "type": "object",
   "required": [
    "journey_id",
    "name",
    "status",
    "risk",
    "goal_id",
    "coverage_ref",
    "step_count",
    "assertion_count",
    "approved_by"
   ],
   "properties": [
    {
     "name": "journey_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "risk",
     "type": "string",
     "ref": null
    },
    {
     "name": "goal_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "coverage_ref",
     "type": "string",
     "ref": null
    },
    {
     "name": "step_count",
     "type": "integer",
     "ref": null
    },
    {
     "name": "assertion_count",
     "type": "integer",
     "ref": null
    },
    {
     "name": "approved_by",
     "type": "string",
     "ref": null
    },
    {
     "name": "approved_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthCapabilityCoverage",
   "type": "object",
   "required": [
    "capability",
    "journey_ids",
    "status_counts"
   ],
   "properties": [
    {
     "name": "capability",
     "type": "string",
     "ref": null
    },
    {
     "name": "journey_ids",
     "type": "array",
     "ref": null
    },
    {
     "name": "status_counts",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthComparisonReport",
   "type": "object",
   "required": [
    "stable_version",
    "candidate_version",
    "stable_runs",
    "candidate_runs",
    "stable_completion_pct",
    "candidate_completion_pct",
    "journey_completion_delta_pp",
    "latency_delta",
    "assertion_pass_delta_pp",
    "console_error_delta",
    "version_drift",
    "meaningful",
    "reasons",
    "thresholds"
   ],
   "properties": [
    {
     "name": "stable_version",
     "type": "string",
     "ref": null
    },
    {
     "name": "candidate_version",
     "type": "string",
     "ref": null
    },
    {
     "name": "stable_runs",
     "type": "integer",
     "ref": null
    },
    {
     "name": "candidate_runs",
     "type": "integer",
     "ref": null
    },
    {
     "name": "stable_completion_pct",
     "type": "number",
     "ref": null
    },
    {
     "name": "candidate_completion_pct",
     "type": "number",
     "ref": null
    },
    {
     "name": "journey_completion_delta_pp",
     "type": "number",
     "ref": null
    },
    {
     "name": "latency_delta",
     "type": "SynthLatencyDelta",
     "ref": "SynthLatencyDelta"
    },
    {
     "name": "stable_assertion_pass_pct",
     "type": "number",
     "ref": null
    },
    {
     "name": "candidate_assertion_pass_pct",
     "type": "number",
     "ref": null
    },
    {
     "name": "assertion_pass_delta_pp",
     "type": "number",
     "ref": null
    },
    {
     "name": "stable_console_errors_per_run",
     "type": "number",
     "ref": null
    },
    {
     "name": "candidate_console_errors_per_run",
     "type": "number",
     "ref": null
    },
    {
     "name": "console_error_delta",
     "type": "number",
     "ref": null
    },
    {
     "name": "version_drift",
     "type": "array",
     "ref": null
    },
    {
     "name": "meaningful",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "reasons",
     "type": "array",
     "ref": null
    },
    {
     "name": "thresholds",
     "type": "SynthComparisonThresholds",
     "ref": "SynthComparisonThresholds"
    }
   ]
  },
  {
   "name": "SynthComparisonThresholds",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "completion_delta_pp",
     "type": "number",
     "ref": null
    },
    {
     "name": "latency_p50_rel_pct",
     "type": "number",
     "ref": null
    },
    {
     "name": "latency_p95_rel_pct",
     "type": "number",
     "ref": null
    },
    {
     "name": "assertion_pass_delta_pp",
     "type": "number",
     "ref": null
    },
    {
     "name": "console_error_per_run_abs",
     "type": "number",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthDeletedResponse",
   "type": "object",
   "required": [
    "deleted"
   ],
   "properties": [
    {
     "name": "deleted",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthEnvironment",
   "type": "object",
   "required": [
    "id",
    "org_id",
    "target_id",
    "kind",
    "mode",
    "allowed_domains",
    "rate_cap_per_min",
    "budget_per_run",
    "quiet_windows",
    "test_windows",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "target_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "kind",
     "type": "string",
     "ref": null
    },
    {
     "name": "mode",
     "type": "string",
     "ref": null
    },
    {
     "name": "allowed_domains",
     "type": "array",
     "ref": null
    },
    {
     "name": "rate_cap_per_min",
     "type": "integer",
     "ref": null
    },
    {
     "name": "budget_per_run",
     "type": "integer",
     "ref": null
    },
    {
     "name": "quiet_windows",
     "type": "array",
     "ref": null
    },
    {
     "name": "test_windows",
     "type": "array",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthEnvironmentListResponse",
   "type": "object",
   "required": [
    "environments"
   ],
   "properties": [
    {
     "name": "environments",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthEnvironmentResponse",
   "type": "object",
   "required": [
    "environment"
   ],
   "properties": [
    {
     "name": "environment",
     "type": "SynthEnvironment",
     "ref": "SynthEnvironment"
    }
   ]
  },
  {
   "name": "SynthEnvironmentSpec",
   "type": "object",
   "required": [
    "kind"
   ],
   "properties": [
    {
     "name": "kind",
     "type": "string",
     "ref": null
    },
    {
     "name": "mode",
     "type": "string",
     "ref": null
    },
    {
     "name": "allowed_domains",
     "type": "array",
     "ref": null
    },
    {
     "name": "rate_cap_per_min",
     "type": "integer",
     "ref": null
    },
    {
     "name": "budget_per_run",
     "type": "integer",
     "ref": null
    },
    {
     "name": "quiet_windows",
     "type": "array",
     "ref": null
    },
    {
     "name": "test_windows",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthEvidenceBundle",
   "type": "object",
   "required": [
    "schema_version",
    "bundle_id",
    "org_id",
    "run_id",
    "captured_at"
   ],
   "properties": [
    {
     "name": "schema_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "bundle_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "project_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_kind",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_status",
     "type": "string",
     "ref": null
    },
    {
     "name": "journey_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "journey_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "persona_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "persona_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "environment_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "environment_kind",
     "type": "string",
     "ref": null
    },
    {
     "name": "environment_mode",
     "type": "string",
     "ref": null
    },
    {
     "name": "app_version",
     "type": "string",
     "ref": null
    },
    {
     "name": "captured_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_started_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_finished_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "failing_assertion",
     "type": "SynthAssertionOutcome",
     "ref": "SynthAssertionOutcome"
    },
    {
     "name": "assertion_results",
     "type": "array",
     "ref": null
    },
    {
     "name": "repro_steps",
     "type": "array",
     "ref": null
    },
    {
     "name": "screenshot_refs",
     "type": "array",
     "ref": null
    },
    {
     "name": "browser_trace_ref",
     "type": "string",
     "ref": null
    },
    {
     "name": "console_refs",
     "type": "array",
     "ref": null
    },
    {
     "name": "network_refs",
     "type": "array",
     "ref": null
    },
    {
     "name": "other_refs",
     "type": "array",
     "ref": null
    },
    {
     "name": "correlation_ids",
     "type": "object",
     "ref": null
    },
    {
     "name": "redacted_meta",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthExplorationAction",
   "type": "object",
   "required": [
    "kind",
    "write_op"
   ],
   "properties": [
    {
     "name": "kind",
     "type": "string",
     "ref": null
    },
    {
     "name": "url",
     "type": "string",
     "ref": null
    },
    {
     "name": "target",
     "type": "string",
     "ref": null
    },
    {
     "name": "value",
     "type": "string",
     "ref": null
    },
    {
     "name": "write_op",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "synthetic_account",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthExplorationCost",
   "type": "object",
   "required": [
    "steps",
    "denied",
    "duration_ms"
   ],
   "properties": [
    {
     "name": "steps",
     "type": "integer",
     "ref": null
    },
    {
     "name": "denied",
     "type": "integer",
     "ref": null
    },
    {
     "name": "duration_ms",
     "type": "number",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthExplorationObservation",
   "type": "object",
   "required": [
    "url",
    "at"
   ],
   "properties": [
    {
     "name": "url",
     "type": "string",
     "ref": null
    },
    {
     "name": "title",
     "type": "string",
     "ref": null
    },
    {
     "name": "links",
     "type": "array",
     "ref": null
    },
    {
     "name": "content",
     "type": "string",
     "ref": null
    },
    {
     "name": "at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthExplorationPlan",
   "type": "object",
   "required": [
    "id",
    "org_id",
    "project_id",
    "environment_id",
    "persona_id",
    "steps",
    "created_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "project_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "environment_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "persona_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "target_base_url",
     "type": "string",
     "ref": null
    },
    {
     "name": "goal_ids",
     "type": "array",
     "ref": null
    },
    {
     "name": "steps",
     "type": "array",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthExplorationPlanStep",
   "type": "object",
   "required": [
    "index",
    "path",
    "goal_id"
   ],
   "properties": [
    {
     "name": "index",
     "type": "integer",
     "ref": null
    },
    {
     "name": "path",
     "type": "string",
     "ref": null
    },
    {
     "name": "goal_id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthFinding",
   "type": "object",
   "required": [
    "id",
    "org_id",
    "run_id",
    "status",
    "severity",
    "fingerprint",
    "confidence",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "journey_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "assertion_ix",
     "type": "integer",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "severity",
     "type": "string",
     "ref": null
    },
    {
     "name": "title",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "fingerprint",
     "type": "string",
     "ref": null
    },
    {
     "name": "repro_steps",
     "type": "string",
     "ref": null
    },
    {
     "name": "evidence_refs",
     "type": "array",
     "ref": null
    },
    {
     "name": "repro_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "reproducible",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "confidence",
     "type": "number",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthGitHubRef",
   "type": "object",
   "required": [
    "id",
    "org_id",
    "finding_id",
    "fingerprint",
    "repo",
    "issue_number",
    "issue_url",
    "idempotency_key",
    "synced_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "finding_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "fingerprint",
     "type": "string",
     "ref": null
    },
    {
     "name": "repo",
     "type": "string",
     "ref": null
    },
    {
     "name": "issue_number",
     "type": "integer",
     "ref": null
    },
    {
     "name": "issue_url",
     "type": "string",
     "ref": null
    },
    {
     "name": "idempotency_key",
     "type": "string",
     "ref": null
    },
    {
     "name": "synced_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthJourney",
   "type": "object",
   "required": [
    "id",
    "org_id",
    "project_id",
    "environment_id",
    "persona_id",
    "name",
    "status",
    "risk",
    "version",
    "steps",
    "assertions"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "project_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "environment_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "persona_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "risk",
     "type": "string",
     "ref": null
    },
    {
     "name": "version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "preconditions",
     "type": "array",
     "ref": null
    },
    {
     "name": "steps",
     "type": "array",
     "ref": null
    },
    {
     "name": "assertions",
     "type": "array",
     "ref": null
    },
    {
     "name": "cleanup",
     "type": "array",
     "ref": null
    },
    {
     "name": "tags",
     "type": "array",
     "ref": null
    },
    {
     "name": "source",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthJourneyCoverage",
   "type": "object",
   "required": [
    "total_journeys",
    "untagged_journey_ids",
    "capabilities"
   ],
   "properties": [
    {
     "name": "total_journeys",
     "type": "integer",
     "ref": null
    },
    {
     "name": "untagged_journey_ids",
     "type": "array",
     "ref": null
    },
    {
     "name": "capabilities",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthJourneyStep",
   "type": "object",
   "required": [
    "index",
    "action",
    "target",
    "write_op"
   ],
   "properties": [
    {
     "name": "index",
     "type": "integer",
     "ref": null
    },
    {
     "name": "action",
     "type": "string",
     "ref": null
    },
    {
     "name": "target",
     "type": "string",
     "ref": null
    },
    {
     "name": "value",
     "type": "string",
     "ref": null
    },
    {
     "name": "write_op",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "timeout_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "hint",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthLatencyDelta",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "stable_p50_ms",
     "type": "number",
     "ref": null
    },
    {
     "name": "stable_p95_ms",
     "type": "number",
     "ref": null
    },
    {
     "name": "candidate_p50_ms",
     "type": "number",
     "ref": null
    },
    {
     "name": "candidate_p95_ms",
     "type": "number",
     "ref": null
    },
    {
     "name": "p50_delta_pct",
     "type": "number",
     "ref": null
    },
    {
     "name": "p95_delta_pct",
     "type": "number",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthOccurrence",
   "type": "object",
   "required": [
    "run_id",
    "at"
   ],
   "properties": [
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthPersona",
   "type": "object",
   "required": [
    "id",
    "org_id",
    "name",
    "role_hints",
    "constraints",
    "version"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "built_in",
     "type": "string",
     "ref": null
    },
    {
     "name": "role_hints",
     "type": "array",
     "ref": null
    },
    {
     "name": "constraints",
     "type": "object",
     "ref": null
    },
    {
     "name": "version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthPersonaListResponse",
   "type": "object",
   "required": [
    "personas"
   ],
   "properties": [
    {
     "name": "personas",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthPersonaResponse",
   "type": "object",
   "required": [
    "persona"
   ],
   "properties": [
    {
     "name": "persona",
     "type": "SynthPersona",
     "ref": "SynthPersona"
    }
   ]
  },
  {
   "name": "SynthPersonaSpec",
   "type": "object",
   "required": [
    "name"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "built_in",
     "type": "string",
     "ref": null
    },
    {
     "name": "role_hints",
     "type": "array",
     "ref": null
    },
    {
     "name": "constraints",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthProductionSchedule",
   "type": "object",
   "required": [
    "id",
    "org_id",
    "project_id",
    "journey_id",
    "journey_version",
    "environment_id",
    "persona_id",
    "has_write_ops",
    "paused",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "project_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "journey_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "journey_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "environment_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "persona_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "persona_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "has_write_ops",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "interval_seconds",
     "type": "number",
     "ref": null
    },
    {
     "name": "on_deployment",
     "type": "SynthAfterDeployment",
     "ref": "SynthAfterDeployment"
    },
    {
     "name": "retry",
     "type": "SynthRetryPolicy",
     "ref": "SynthRetryPolicy"
    },
    {
     "name": "paused",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "synthetic_account",
     "type": "string",
     "ref": null
    },
    {
     "name": "next_run_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "attempts",
     "type": "integer",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthProject",
   "type": "object",
   "required": [
    "id",
    "org_id",
    "name",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthProjectCreateRequest",
   "type": "object",
   "required": [
    "name"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthProjectListResponse",
   "type": "object",
   "required": [
    "projects"
   ],
   "properties": [
    {
     "name": "projects",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthProjectResponse",
   "type": "object",
   "required": [
    "project"
   ],
   "properties": [
    {
     "name": "project",
     "type": "SynthProject",
     "ref": "SynthProject"
    }
   ]
  },
  {
   "name": "SynthQuietWindow",
   "type": "object",
   "required": [
    "weekdays",
    "start",
    "end",
    "location"
   ],
   "properties": [
    {
     "name": "weekdays",
     "type": "array",
     "ref": null
    },
    {
     "name": "start",
     "type": "string",
     "ref": null
    },
    {
     "name": "end",
     "type": "string",
     "ref": null
    },
    {
     "name": "location",
     "type": "string",
     "ref": null
    },
    {
     "name": "reason",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthReproResult",
   "type": "object",
   "required": [
    "repro_run_id",
    "reproduced",
    "at"
   ],
   "properties": [
    {
     "name": "repro_run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "reproduced",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "at",
     "type": "string",
     "ref": null
    },
    {
     "name": "environment_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "mode",
     "type": "string",
     "ref": null
    },
    {
     "name": "read_only_subset",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "note",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthRetryPolicy",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "max_attempts",
     "type": "integer",
     "ref": null
    },
    {
     "name": "backoff_seconds",
     "type": "number",
     "ref": null
    },
    {
     "name": "max_backoff_seconds",
     "type": "number",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthRunOptions",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "app_version",
     "type": "string",
     "ref": null
    },
    {
     "name": "max_steps",
     "type": "integer",
     "ref": null
    },
    {
     "name": "kill_switch",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthRunStatus",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "SynthScopeDecision",
   "type": "object",
   "required": [
    "allowed",
    "reason",
    "at"
   ],
   "properties": [
    {
     "name": "allowed",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "reason",
     "type": "string",
     "ref": null
    },
    {
     "name": "at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthScopeLedger",
   "type": "object",
   "required": [
    "planned_routes",
    "discovered_routes",
    "tested_routes",
    "gaps",
    "uncertainty",
    "flags",
    "visited_states",
    "coverage_percent",
    "cost"
   ],
   "properties": [
    {
     "name": "planned_routes",
     "type": "array",
     "ref": null
    },
    {
     "name": "discovered_routes",
     "type": "array",
     "ref": null
    },
    {
     "name": "tested_routes",
     "type": "array",
     "ref": null
    },
    {
     "name": "gaps",
     "type": "array",
     "ref": null
    },
    {
     "name": "uncertainty",
     "type": "array",
     "ref": null
    },
    {
     "name": "flags",
     "type": "array",
     "ref": null
    },
    {
     "name": "visited_states",
     "type": "integer",
     "ref": null
    },
    {
     "name": "coverage_percent",
     "type": "number",
     "ref": null
    },
    {
     "name": "cost",
     "type": "SynthExplorationCost",
     "ref": "SynthExplorationCost"
    }
   ]
  },
  {
   "name": "SynthStepEvent",
   "type": "object",
   "required": [
    "index",
    "step_index",
    "type",
    "status"
   ],
   "properties": [
    {
     "name": "index",
     "type": "integer",
     "ref": null
    },
    {
     "name": "step_index",
     "type": "integer",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "message",
     "type": "string",
     "ref": null
    },
    {
     "name": "redacted_meta",
     "type": "object",
     "ref": null
    },
    {
     "name": "evidence_ref",
     "type": "string",
     "ref": null
    },
    {
     "name": "at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthSyncResult",
   "type": "object",
   "required": [
    "github_ref",
    "created",
    "updated",
    "unchanged"
   ],
   "properties": [
    {
     "name": "github_ref",
     "type": "SynthGitHubRef",
     "ref": "SynthGitHubRef"
    },
    {
     "name": "created",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "updated",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "unchanged",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthTarget",
   "type": "object",
   "required": [
    "id",
    "org_id",
    "project_id",
    "name",
    "base_url",
    "verified",
    "verified_at",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "org_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "project_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "base_url",
     "type": "string",
     "ref": null
    },
    {
     "name": "verified",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "verified_at",
     "type": "['string', 'null']",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthTargetCreateRequest",
   "type": "object",
   "required": [
    "project_id",
    "name",
    "base_url"
   ],
   "properties": [
    {
     "name": "project_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "base_url",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthTargetListResponse",
   "type": "object",
   "required": [
    "targets"
   ],
   "properties": [
    {
     "name": "targets",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthTargetResponse",
   "type": "object",
   "required": [
    "target"
   ],
   "properties": [
    {
     "name": "target",
     "type": "SynthTarget",
     "ref": "SynthTarget"
    }
   ]
  },
  {
   "name": "SynthTargetUpdateRequest",
   "type": "object",
   "required": [
    "name"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthVerification",
   "type": "object",
   "required": [
    "token",
    "instructions"
   ],
   "properties": [
    {
     "name": "token",
     "type": "string",
     "ref": null
    },
    {
     "name": "instructions",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthVerificationResult",
   "type": "object",
   "required": [
    "finding_id",
    "fingerprint",
    "journey_id",
    "outcome",
    "run_id",
    "evidence_refs",
    "detail",
    "checked_at"
   ],
   "properties": [
    {
     "name": "finding_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "fingerprint",
     "type": "string",
     "ref": null
    },
    {
     "name": "journey_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "outcome",
     "type": "string",
     "ref": null
    },
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "evidence_refs",
     "type": "array",
     "ref": null
    },
    {
     "name": "detail",
     "type": "string",
     "ref": null
    },
    {
     "name": "checked_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthVerifyRequest",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "action",
     "type": "string",
     "ref": null
    },
    {
     "name": "token",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthVerifyResponse",
   "type": "object",
   "required": [
    "status",
    "target"
   ],
   "properties": [
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "target",
     "type": "SynthTarget",
     "ref": "SynthTarget"
    },
    {
     "name": "verification",
     "type": "-",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthWorkerHealth",
   "type": "object",
   "required": [
    "kind",
    "healthy",
    "in_flight"
   ],
   "properties": [
    {
     "name": "kind",
     "type": "SynthWorkerKind",
     "ref": "SynthWorkerKind"
    },
    {
     "name": "healthy",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "in_flight",
     "type": "integer",
     "ref": null
    },
    {
     "name": "last_heartbeat",
     "type": "string",
     "ref": null
    },
    {
     "name": "stale",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "SynthWorkerKind",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "Task",
   "type": "object",
   "required": [
    "ID",
    "Type",
    "Payload",
    "Status",
    "Attempts",
    "CreatedAt",
    "UpdatedAt",
    "LastError"
   ],
   "properties": [
    {
     "name": "ID",
     "type": "string",
     "ref": null
    },
    {
     "name": "Type",
     "type": "string",
     "ref": null
    },
    {
     "name": "Payload",
     "type": "object",
     "ref": null
    },
    {
     "name": "Status",
     "type": "string",
     "ref": null
    },
    {
     "name": "Attempts",
     "type": "integer",
     "ref": null
    },
    {
     "name": "CreatedAt",
     "type": "string",
     "ref": null
    },
    {
     "name": "UpdatedAt",
     "type": "string",
     "ref": null
    },
    {
     "name": "LastError",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "ToolCatalogErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ToolCommonArgs",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "estimated_cost_cents",
     "type": "integer",
     "ref": null
    },
    {
     "name": "timeout_ms",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "ToolErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ToolInfo",
   "type": "object",
   "required": [
    "name"
   ],
   "properties": [
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "input_schema",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "ToolListResponse",
   "type": "object",
   "required": [
    "tools"
   ],
   "properties": [
    {
     "name": "tools",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "TrErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "TrRunTraceResponse",
   "type": "object",
   "required": [
    "run_id",
    "trace_id"
   ],
   "properties": [
    {
     "name": "run_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "trace_id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "User",
   "type": "object",
   "required": [
    "ID",
    "Organization",
    "Email",
    "Role",
    "CreatedAt"
   ],
   "properties": [
    {
     "name": "ID",
     "type": "string",
     "ref": null
    },
    {
     "name": "Organization",
     "type": "string",
     "ref": null
    },
    {
     "name": "Email",
     "type": "string",
     "ref": null
    },
    {
     "name": "PasswordHash",
     "type": "string",
     "ref": null
    },
    {
     "name": "Role",
     "type": "string",
     "ref": null
    },
    {
     "name": "CreatedAt",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "UsgCostBucket",
   "type": "object",
   "required": [
    "cost_cents",
    "runs"
   ],
   "properties": [
    {
     "name": "bucket",
     "type": "string",
     "ref": null
    },
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "model",
     "type": "string",
     "ref": null
    },
    {
     "name": "cost_cents",
     "type": "number",
     "ref": null
    },
    {
     "name": "runs",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "UsgErrorEnvelope",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "UsgUsageCostsResponse",
   "type": "object",
   "required": [
    "total_cost_cents",
    "series"
   ],
   "properties": [
    {
     "name": "total_cost_cents",
     "type": "number",
     "ref": null
    },
    {
     "name": "series",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "ValidationErrorItem",
   "type": "object",
   "required": [
    "code",
    "message"
   ],
   "properties": [
    {
     "name": "code",
     "type": "string",
     "ref": null
    },
    {
     "name": "message",
     "type": "string",
     "ref": null
    },
    {
     "name": "node_id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "VdfDiffValue",
   "type": "['string', 'number', 'boolean', 'array', 'object', 'null']",
   "required": [],
   "properties": []
  },
  {
   "name": "VdfErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "VdfVersionDiffField",
   "type": "object",
   "required": [
    "field",
    "from",
    "to",
    "changed"
   ],
   "properties": [
    {
     "name": "field",
     "type": "string",
     "ref": null
    },
    {
     "name": "from",
     "type": "VdfDiffValue",
     "ref": "VdfDiffValue"
    },
    {
     "name": "to",
     "type": "VdfDiffValue",
     "ref": "VdfDiffValue"
    },
    {
     "name": "changed",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "VdfVersionDiffResponse",
   "type": "object",
   "required": [
    "agent_id",
    "from",
    "to",
    "fields"
   ],
   "properties": [
    {
     "name": "agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "from",
     "type": "integer",
     "ref": null
    },
    {
     "name": "to",
     "type": "integer",
     "ref": null
    },
    {
     "name": "fields",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "VersionNumberResponse",
   "type": "object",
   "required": [
    "version"
   ],
   "properties": [
    {
     "name": "version",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "Webhook",
   "type": "object",
   "required": [
    "id",
    "url",
    "events",
    "status",
    "secret_set",
    "created_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "url",
     "type": "string",
     "ref": null
    },
    {
     "name": "events",
     "type": "array",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "secret_set",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "WebhookDeleteResponse",
   "type": "object",
   "required": [
    "deleted"
   ],
   "properties": [
    {
     "name": "deleted",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "WebhookDelivery",
   "type": "object",
   "required": [
    "id",
    "event_type",
    "status",
    "attempts",
    "last_status_code",
    "latency_ms",
    "error",
    "created_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "event_type",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "attempts",
     "type": "integer",
     "ref": null
    },
    {
     "name": "last_status_code",
     "type": "integer",
     "ref": null
    },
    {
     "name": "latency_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "error",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "WebhookDeliveryList",
   "type": "object",
   "required": [
    "deliveries"
   ],
   "properties": [
    {
     "name": "deliveries",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "WebhookError",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "WebhookInput",
   "type": "object",
   "required": [
    "url"
   ],
   "properties": [
    {
     "name": "url",
     "type": "string",
     "ref": null
    },
    {
     "name": "method",
     "type": "string",
     "ref": null
    },
    {
     "name": "body",
     "type": "string",
     "ref": null
    },
    {
     "name": "headers",
     "type": "object",
     "ref": null
    },
    {
     "name": "fire_and_forget",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "timeout_ms",
     "type": "integer",
     "ref": null
    },
    {
     "name": "estimated_cost_cents",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "WebhookList",
   "type": "object",
   "required": [
    "webhooks"
   ],
   "properties": [
    {
     "name": "webhooks",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "WebhookOutput",
   "type": "object",
   "required": [
    "delivered",
    "status",
    "signed"
   ],
   "properties": [
    {
     "name": "delivered",
     "type": "boolean",
     "ref": null
    },
    {
     "name": "status",
     "type": "integer",
     "ref": null
    },
    {
     "name": "body",
     "type": "string",
     "ref": null
    },
    {
     "name": "signed",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "Wf3ErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "Wf3NodeTimelineResponse",
   "type": "object",
   "required": [
    "nodes"
   ],
   "properties": [
    {
     "name": "nodes",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "Wf3NodeTimelineStatus",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "Wf3WorkflowRunNode",
   "type": "object",
   "required": [
    "id",
    "node_id",
    "status",
    "attempt",
    "started_at",
    "finished_at",
    "error_code"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "node_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "Wf3NodeTimelineStatus",
     "ref": "Wf3NodeTimelineStatus"
    },
    {
     "name": "attempt",
     "type": "integer",
     "ref": null
    },
    {
     "name": "started_at",
     "type": "['string', 'null']",
     "ref": null
    },
    {
     "name": "finished_at",
     "type": "['string', 'null']",
     "ref": null
    },
    {
     "name": "error_code",
     "type": "['string', 'null']",
     "ref": null
    }
   ]
  },
  {
   "name": "WfAggregationStrategy",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "WfConditionNode",
   "type": "object",
   "required": [
    "id",
    "type"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "config",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "WfDSL",
   "type": "object",
   "required": [
    "nodes"
   ],
   "properties": [
    {
     "name": "nodes",
     "type": "array",
     "ref": null
    },
    {
     "name": "edges",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "WfEdge",
   "type": "object",
   "required": [
    "from",
    "to"
   ],
   "properties": [
    {
     "name": "from",
     "type": "string",
     "ref": null
    },
    {
     "name": "to",
     "type": "string",
     "ref": null
    },
    {
     "name": "condition",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "WfErrorEnvelope",
   "type": "object",
   "required": [
    "error"
   ],
   "properties": [
    {
     "name": "error",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "WfExpression",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "WfLoopNode",
   "type": "object",
   "required": [
    "id",
    "type"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "config",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "WfNode",
   "type": "object",
   "required": [
    "id",
    "type"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "config",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "WfParallelLimits",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "global",
     "type": "integer",
     "ref": null
    },
    {
     "name": "per_workflow",
     "type": "integer",
     "ref": null
    },
    {
     "name": "org_quota",
     "type": "integer",
     "ref": null
    },
    {
     "name": "effective",
     "type": "integer",
     "ref": null
    }
   ]
  },
  {
   "name": "WfParallelNode",
   "type": "object",
   "required": [
    "id",
    "type"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "config",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "WfRetryNode",
   "type": "object",
   "required": [
    "id",
    "type"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "config",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "WfSwitchNode",
   "type": "object",
   "required": [
    "id",
    "type"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "config",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "WfValidateFailure",
   "type": "object",
   "required": [
    "errors"
   ],
   "properties": [
    {
     "name": "errors",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "WfValidateRequest",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "dsl",
     "type": "WfDSL",
     "ref": "WfDSL"
    }
   ]
  },
  {
   "name": "WfValidateSuccess",
   "type": "object",
   "required": [
    "valid"
   ],
   "properties": [
    {
     "name": "valid",
     "type": "boolean",
     "ref": null
    }
   ]
  },
  {
   "name": "WfWaitNode",
   "type": "object",
   "required": [
    "id",
    "type"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "type",
     "type": "string",
     "ref": null
    },
    {
     "name": "config",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "WfWorkflowValidationError",
   "type": "object",
   "required": [
    "code",
    "message"
   ],
   "properties": [
    {
     "name": "code",
     "type": "string",
     "ref": null
    },
    {
     "name": "message",
     "type": "string",
     "ref": null
    },
    {
     "name": "node_id",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "WorkerAttempt",
   "type": "object",
   "required": [],
   "properties": [
    {
     "name": "worker_agent_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "delegation_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "error",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "WorkflowDetail",
   "type": "object",
   "required": [],
   "properties": []
  },
  {
   "name": "WorkflowDetailResponse",
   "type": "object",
   "required": [
    "workflow"
   ],
   "properties": [
    {
     "name": "workflow",
     "type": "WorkflowDetail",
     "ref": "WorkflowDetail"
    }
   ]
  },
  {
   "name": "WorkflowDetailWithVersionsResponse",
   "type": "object",
   "required": [
    "workflow"
   ],
   "properties": [
    {
     "name": "workflow",
     "type": "object",
     "ref": null
    }
   ]
  },
  {
   "name": "WorkflowDSL",
   "type": "object",
   "required": [
    "nodes",
    "edges"
   ],
   "properties": [
    {
     "name": "nodes",
     "type": "array",
     "ref": null
    },
    {
     "name": "edges",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "WorkflowListResponse",
   "type": "object",
   "required": [
    "workflows"
   ],
   "properties": [
    {
     "name": "workflows",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "WorkflowRun",
   "type": "object",
   "required": [
    "id",
    "workflow_id",
    "status",
    "node_runs"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "workflow_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "WorkflowRunStatus",
     "ref": "WorkflowRunStatus"
    },
    {
     "name": "node_runs",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "WorkflowRunResponse",
   "type": "object",
   "required": [
    "id",
    "workflow_id",
    "status",
    "node_runs"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "workflow_id",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "WorkflowRunStatus",
     "ref": "WorkflowRunStatus"
    },
    {
     "name": "node_runs",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "WorkflowRunStatus",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "WorkflowStatus",
   "type": "string",
   "required": [],
   "properties": []
  },
  {
   "name": "WorkflowSummary",
   "type": "object",
   "required": [
    "id",
    "name",
    "status",
    "current_version",
    "created_at",
    "updated_at"
   ],
   "properties": [
    {
     "name": "id",
     "type": "string",
     "ref": null
    },
    {
     "name": "name",
     "type": "string",
     "ref": null
    },
    {
     "name": "description",
     "type": "string",
     "ref": null
    },
    {
     "name": "status",
     "type": "WorkflowStatus",
     "ref": "WorkflowStatus"
    },
    {
     "name": "current_version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "updated_at",
     "type": "string",
     "ref": null
    }
   ]
  },
  {
   "name": "WorkflowValidationErrors",
   "type": "object",
   "required": [
    "errors"
   ],
   "properties": [
    {
     "name": "errors",
     "type": "array",
     "ref": null
    }
   ]
  },
  {
   "name": "WorkflowVersion",
   "type": "object",
   "required": [
    "version",
    "status",
    "created_at",
    "dsl_snapshot"
   ],
   "properties": [
    {
     "name": "version",
     "type": "integer",
     "ref": null
    },
    {
     "name": "status",
     "type": "string",
     "ref": null
    },
    {
     "name": "created_at",
     "type": "string",
     "ref": null
    },
    {
     "name": "dsl_snapshot",
     "type": "WorkflowDSL",
     "ref": "WorkflowDSL"
    }
   ]
  }
 ]
};
