package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// testCorpus is built so the rule-based generator's top proper nouns are
// deterministic: Payments (3x), Deployments (2x), Acme/Robotics (1x each).
const testCorpus = `Acme Robotics builds robots. Payments handles money. Payments again. ` +
	`Deployments ship robots. Payments and Deployments tie together.`

// validTestOntology is a hand-written schema used by the extraction tests.
func validTestOntology() *Ontology {
	personAttrs := CanonicalAttributes()
	personAttrs = append(personAttrs, AttributeDef{Name: "title", Type: AttrTypeString})
	orgAttrs := CanonicalAttributes()
	orgAttrs = append(orgAttrs, AttributeDef{Name: "headcount", Type: AttrTypeInteger})
	return &Ontology{
		OrganizationID: "org-1",
		EntityTypes: []EntityType{
			{Name: "Person", Attributes: personAttrs, Fallback: true, Cues: append([]string(nil), builtinCues["Person"]...)},
			{Name: "Organization", Attributes: orgAttrs, Fallback: true, Cues: append([]string(nil), builtinCues["Organization"]...)},
		},
		EdgeTypes: []EdgeType{
			{Name: "WORKS_FOR", SourceType: "Person", TargetType: "Organization"},
		},
		AttributeDefs: CanonicalAttributes(),
	}
}

// ---------------------------------------------------------------------------
// Naming-convention enforcement (typed validation errors).
// ---------------------------------------------------------------------------

func TestValidateOntologyAcceptsValidSchema(t *testing.T) {
	if err := ValidateOntology(validTestOntology()); err != nil {
		t.Fatalf("valid ontology rejected: %v", err)
	}
	if err := ValidateOntology(nil); !errors.Is(err, ErrOntologyValidation) {
		t.Fatalf("nil ontology must be a validation error, got %v", err)
	}
}

func TestValidateOntologyCollectsNamingViolations(t *testing.T) {
	ont := &Ontology{
		EntityTypes: []EntityType{
			{Name: "invoice", Attributes: []AttributeDef{{Name: "Total", Type: "text"}, {Name: "total", Type: "text"}, {Name: "uuid", Type: AttrTypeNumber}}},
			{Name: "Invoice", Attributes: []AttributeDef{{Name: "ok_name", Type: AttrTypeString}}},
		},
		EdgeTypes: []EdgeType{
			{Name: "billed-to", SourceType: "Invoice", TargetType: "Missing"},
			{Name: "BILLED_TO", SourceType: "Invoice", TargetType: "Missing"},
			{Name: "SELF_LOOP", SourceType: "Invoice", TargetType: "Invoice"},
		},
	}
	err := ValidateOntology(ont)
	if !errors.Is(err, ErrOntologyValidation) {
		t.Fatalf("want ErrOntologyValidation, got %v", err)
	}
	var valErr *OntologyValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("want *OntologyValidationError, got %T", err)
	}
	fields := make(map[string]string)
	for _, f := range valErr.Fields {
		fields[f.Field] = f.Issue
	}
	// ALL violations collected (never fail-fast, never silent):
	for _, field := range []string{
		"entity_types[0].name",               // not PascalCase
		"entity_types[1].name",               // duplicate (case-insensitive)
		"entity_types[0].attributes[0].name", // not snake_case
		"entity_types[0].attributes[0].type", // unknown type
		"entity_types[0].attributes[1].name", // duplicate attribute
		"entity_types[0].attributes[2].type", // reserved uuid redefined
		"edge_types[0].name",                 // not UPPER_SNAKE_CASE
		"edge_types[0].target_type",          // unknown entity type
		"edge_types[1].target_type",          // unknown entity type
		"edge_types[2]",                      // self-loop
	} {
		if _, ok := fields[field]; !ok {
			t.Fatalf("missing field error for %s; got %v", field, valErr.Fields)
		}
	}
}

func TestValidateOntologyReservedAttributeTypeEnforced(t *testing.T) {
	ont := &Ontology{
		EntityTypes: []EntityType{{Name: "Thing", Attributes: CanonicalAttributes()}},
	}
	if err := ValidateOntology(ont); err != nil {
		t.Fatalf("canonical reserved attributes must validate: %v", err)
	}
	ont.EntityTypes[0].Attributes = append(ont.EntityTypes[0].Attributes,
		AttributeDef{Name: "created_at", Type: AttrTypeInteger})
	err := ValidateOntology(ont)
	if !errors.Is(err, ErrOntologyValidation) {
		t.Fatalf("redefining created_at as integer must fail, got %v", err)
	}
	if !strings.Contains(err.Error(), "created_at") {
		t.Fatalf("error must name the reserved attribute: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Generation: fallback guarantee + determinism.
// ---------------------------------------------------------------------------

func TestRuleBasedGeneratorFallbackGuarantee(t *testing.T) {
	gen := NewRuleBasedGenerator()
	// Empty corpus: no candidates at all, yet the fallback types exist.
	ont, err := gen.GenerateOntology(context.Background(), "", GenerateOptions{})
	if err != nil {
		t.Fatalf("GenerateOntology(empty) returned error: %v", err)
	}
	if len(ont.EntityTypes) != 2 {
		t.Fatalf("empty corpus must still yield exactly the fallback types, got %d", len(ont.EntityTypes))
	}
	for _, name := range fallbackTypeNames {
		found := false
		for _, et := range ont.EntityTypes {
			if et.Name == name {
				found = true
				if !et.Fallback {
					t.Fatalf("fallback type %s must carry fallback=true", name)
				}
				for _, res := range reservedAttributeDefs {
					ok := false
					for _, a := range et.Attributes {
						if a.Name == res.Name && a.Type == res.Type {
							ok = true
						}
					}
					if !ok {
						t.Fatalf("fallback type %s missing reserved attribute %s", name, res.Name)
					}
				}
				if len(et.Cues) == 0 {
					t.Fatalf("fallback type %s must ship builtin cues", name)
				}
			}
		}
		if !found {
			t.Fatalf("guaranteed fallback type %s missing", name)
		}
	}
}

func TestRuleBasedGeneratorDeterministicAndTyped(t *testing.T) {
	gen := NewRuleBasedGenerator()
	first, err := gen.GenerateOntology(context.Background(), testCorpus, GenerateOptions{MaxEntityTypes: 2})
	if err != nil {
		t.Fatalf("GenerateOntology returned error: %v", err)
	}
	second, err := gen.GenerateOntology(context.Background(), testCorpus, GenerateOptions{MaxEntityTypes: 2})
	if err != nil {
		t.Fatalf("GenerateOntology returned error: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("rule-based generation must be deterministic for identical corpora")
	}

	// Top-2 by frequency: Payments (3), Deployments (2) — PascalCase kept.
	names := make(map[string]EntityType)
	for _, et := range first.EntityTypes {
		names[et.Name] = et
	}
	for _, want := range []string{"Payments", "Deployments"} {
		et, ok := names[want]
		if !ok {
			t.Fatalf("expected generated entity type %q; got %v", want, first.EntityTypes)
		}
		if et.Fallback {
			t.Fatalf("%q must not be marked fallback", want)
		}
	}
	// Generated edge from co-occurrence, canonical pair order.
	wantEdge := []EdgeType{{Name: "RELATES_TO", SourceType: "Deployments", TargetType: "Payments"}}
	if len(first.EdgeTypes) != 1 || !reflect.DeepEqual(first.EdgeTypes, wantEdge) {
		t.Fatalf("expected %+v edge, got %+v", wantEdge, first.EdgeTypes)
	}
	// Reserved attributes injected into generated types.
	for _, a := range names["Payments"].Attributes {
		if a.Name == "uuid" && a.Type != AttrTypeString {
			t.Fatalf("reserved uuid must be string, got %s", a.Type)
		}
	}
	if first.AttributeDefs == nil || len(first.AttributeDefs) != len(reservedAttributeDefs) {
		t.Fatal("ontology attribute_defs must carry the canonical reserved definitions")
	}
}

func TestRuleBasedGeneratorNamingConventionsAlwaysHold(t *testing.T) {
	gen := NewRuleBasedGenerator()
	ont, err := gen.GenerateOntology(context.Background(), testCorpus, GenerateOptions{})
	if err != nil {
		t.Fatalf("GenerateOntology returned error: %v", err)
	}
	for _, et := range ont.EntityTypes {
		if !IsPascalCase(et.Name) {
			t.Fatalf("generated entity type %q violates PascalCase", et.Name)
		}
	}
	for _, eg := range ont.EdgeTypes {
		if !IsUpperSnakeCase(eg.Name) {
			t.Fatalf("generated edge type %q violates UPPER_SNAKE_CASE", eg.Name)
		}
	}
	for _, ad := range ont.AttributeDefs {
		if !IsSnakeCase(ad.Name) {
			t.Fatalf("generated attribute %q violates snake_case", ad.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// LLM-driven generation via the narrow LLMGateway seam.
// ---------------------------------------------------------------------------

type fakeGateway struct {
	response string
	err      error
	prompt   string
}

func (f *fakeGateway) Generate(_ context.Context, prompt string) (string, error) {
	f.prompt = prompt
	return f.response, f.err
}

func TestLLMOntologyGeneratorValidResponse(t *testing.T) {
	gw := &fakeGateway{response: `{"entity_types":[{"name":"Invoice","attributes":[{"name":"total","type":"number","required":true}]}],"edge_types":[{"name":"BILLED_TO","source_type":"Invoice","target_type":"Organization"}]}`}
	gen := NewLLMOntologyGenerator(gw)
	ont, err := gen.GenerateOntology(context.Background(), "invoices corpus", GenerateOptions{})
	if err != nil {
		t.Fatalf("GenerateOntology returned error: %v", err)
	}
	// Fallback guarantee holds for LLM output too.
	for _, name := range fallbackTypeNames {
		found := false
		for _, et := range ont.EntityTypes {
			if et.Name == name && et.Fallback {
				found = true
			}
		}
		if !found {
			t.Fatalf("fallback type %s not guaranteed by LLM path", name)
		}
	}
	// The proposed schema survived untouched (validated, never renamed).
	if len(ont.EdgeTypes) != 1 || ont.EdgeTypes[0].Name != "BILLED_TO" {
		t.Fatalf("LLM edge types not preserved: %+v", ont.EdgeTypes)
	}
	// Deterministic prompt: same corpus -> same prompt, corpus embedded.
	_, _ = gen.GenerateOntology(context.Background(), "invoices corpus", GenerateOptions{})
	if gw.prompt == "" || !strings.Contains(gw.prompt, "invoices corpus") {
		t.Fatal("prompt must embed the corpus")
	}
}

func TestLLMOntologyGeneratorErrorsCollected(t *testing.T) {
	t.Run("gateway failure wraps", func(t *testing.T) {
		gen := NewLLMOntologyGenerator(&fakeGateway{err: errors.New("timeout")})
		_, err := gen.GenerateOntology(context.Background(), "c", GenerateOptions{})
		if err == nil || !strings.Contains(err.Error(), "ontology LLM generation failed") {
			t.Fatalf("gateway failure must wrap, got %v", err)
		}
	})
	t.Run("invalid JSON", func(t *testing.T) {
		gen := NewLLMOntologyGenerator(&fakeGateway{response: "not json"})
		_, err := gen.GenerateOntology(context.Background(), "c", GenerateOptions{})
		if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
			t.Fatalf("invalid JSON must surface, got %v", err)
		}
	})
	t.Run("schema violation typed", func(t *testing.T) {
		gen := NewLLMOntologyGenerator(&fakeGateway{response: `{"entity_types":[{"name":"invoice","attributes":[{"name":"Total","type":"text"}]}]}`})
		_, err := gen.GenerateOntology(context.Background(), "c", GenerateOptions{})
		if !errors.Is(err, ErrOntologyValidation) {
			t.Fatalf("LLM naming violations must surface as ErrOntologyValidation, got %v", err)
		}
	})
	t.Run("nil gateway", func(t *testing.T) {
		gen := NewLLMOntologyGenerator(nil)
		if _, err := gen.GenerateOntology(context.Background(), "c", GenerateOptions{}); err == nil {
			t.Fatal("nil gateway must be rejected")
		}
	})
}

// ---------------------------------------------------------------------------
// Schema-driven extraction with type validation.
// ---------------------------------------------------------------------------

const extractionText = "Dr. Alice Smith joined Acme Inc. headcount: 42. " +
	"Bob was not typed. Acme Inc headcount: many stays invalid."

func TestExtractEntitiesTypesValidatesAndConnects(t *testing.T) {
	res := ExtractEntities(extractionText, validTestOntology())

	// Typed entities: Person via courtesy-title cue, Organization via the
	// org-suffix cue in the following token; untyped candidates ("Bob") are
	// dropped (no type claims them); duplicates collapse.
	if len(res.Entities) != 2 {
		t.Fatalf("expected 2 typed entities, got %+v", res.Entities)
	}
	person, org := res.Entities[0], res.Entities[1]
	if person.Type != "Person" || person.Name != "Alice Smith" {
		t.Fatalf("person extraction mismatch: %+v", person)
	}
	if org.Type != "Organization" || org.Name != "Acme" {
		t.Fatalf("organization extraction mismatch: %+v", org)
	}

	// Attribute validation: the integer 42 attaches to the nearest
	// preceding Organization mention; "many" is collected as a type error
	// (never silent).
	if got := org.Attributes["headcount"]; got != int64(42) {
		t.Fatalf("headcount = %v (%T), want int64(42)", got, got)
	}
	if len(res.Rejected) != 1 {
		t.Fatalf("expected exactly 1 rejected value, got %+v", res.Rejected)
	}
	if !strings.Contains(res.Rejected[0].Reason, "expects integer") {
		t.Fatalf("rejection reason must name the expected type: %+v", res.Rejected)
	}

	// Edge typed through the declared WORKS_FOR edge only.
	if len(res.Edges) != 1 || res.Edges[0].Type != "WORKS_FOR" ||
		res.Edges[0].Source != "Alice Smith" || res.Edges[0].Target != "Acme" {
		t.Fatalf("edge mismatch: %+v", res.Edges)
	}
}

func TestExtractEntitiesInvalidOntologyCollected(t *testing.T) {
	broken := &Ontology{EntityTypes: []EntityType{{Name: "bad_name"}}}
	res := ExtractEntities("anything", broken)
	if len(res.Entities) != 0 {
		t.Fatalf("invalid ontology must not yield entities, got %+v", res.Entities)
	}
	if len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0].Reason, "validation failed") {
		t.Fatalf("invalid ontology must be collected as rejection: %+v", res.Rejected)
	}
}

func TestQueryEntitiesTypeFilter(t *testing.T) {
	t.Run("filters by type and validates the filter", func(t *testing.T) {
		res := QueryEntities(extractionText, validTestOntology(), "Person")
		if len(res.Entities) != 1 || res.Entities[0].Type != "Person" {
			t.Fatalf("person filter mismatch: %+v", res.Entities)
		}
		if len(res.Edges) != 0 {
			t.Fatalf("edges with filtered endpoints must drop: %+v", res.Edges)
		}
	})
	t.Run("unknown filter type is rejected", func(t *testing.T) {
		res := QueryEntities(extractionText, validTestOntology(), "Robot")
		if len(res.Entities) != 0 {
			t.Fatalf("unknown filter must yield nothing: %+v", res.Entities)
		}
		found := false
		for _, r := range res.Rejected {
			if r.Type == "Robot" && strings.Contains(r.Reason, "not declared") {
				found = true
			}
		}
		if !found {
			t.Fatalf("unknown filter must be collected: %+v", res.Rejected)
		}
	})
}

func TestExtractionDatetimeAttribute(t *testing.T) {
	ont := validTestOntology()
	ont.EntityTypes[1].Attributes = append(ont.EntityTypes[1].Attributes,
		AttributeDef{Name: "founded_on", Type: AttrTypeDatetime})
	res := ExtractEntities("Acme Inc founded_on: 2025-06-01", ont)
	if len(res.Entities) != 1 {
		t.Fatalf("expected Acme, got %+v", res.Entities)
	}
	val, ok := res.Entities[0].Attributes["founded_on"]
	if !ok {
		t.Fatalf("datetime attribute missing: %+v", res.Entities[0].Attributes)
	}
	if !strings.HasPrefix(val.(string), "2025-06-01") {
		t.Fatalf("datetime value = %v", val)
	}
	if len(res.Rejected) != 0 {
		t.Fatalf("unexpected rejections: %+v", res.Rejected)
	}
}

// ---------------------------------------------------------------------------
// Ontology persistence: in-memory + Postgres (sqlmock).
// ---------------------------------------------------------------------------

func TestMemoryOntologyStoreRoundTripAndIsolation(t *testing.T) {
	store := NewInMemoryOntologyStore()
	ctx := context.Background()

	ont := validTestOntology()
	if err := store.SaveOntology(ctx, ont); err != nil {
		t.Fatalf("SaveOntology returned error: %v", err)
	}
	if ont.ID == "" || ont.UpdatedAt.IsZero() {
		t.Fatal("SaveOntology must assign id and updated_at")
	}
	got, err := store.GetOntology(ctx, "org-1", ont.ID)
	if err != nil {
		t.Fatalf("GetOntology returned error: %v", err)
	}
	if !reflect.DeepEqual(got.EntityTypes, ont.EntityTypes) || !reflect.DeepEqual(got.EdgeTypes, ont.EdgeTypes) {
		t.Fatal("schema round-trip mismatch")
	}
	// Tenant guard: foreign org gets "not found".
	if _, err := store.GetOntology(ctx, "org-2", ont.ID); !errors.Is(err, ErrOntologyNotFound) {
		t.Fatalf("foreign org must get ErrOntologyNotFound, got %v", err)
	}
	// List newest first.
	other := &Ontology{OrganizationID: "org-1", EntityTypes: []EntityType{{Name: "Other"}}}
	if err := store.SaveOntology(ctx, other); err != nil {
		t.Fatalf("SaveOntology returned error: %v", err)
	}
	list, err := store.ListOntologies(ctx, "org-1")
	if err != nil || len(list) != 2 {
		t.Fatalf("ListOntologies = %d (err %v), want 2", len(list), err)
	}
	if list[0].ID != other.ID {
		t.Fatal("listing must be newest first")
	}
	if list, _ := store.ListOntologies(ctx, "org-2"); len(list) != 0 {
		t.Fatal("org-2 must see nothing")
	}
}

func newMockOntologyDB(t *testing.T) (*pgOntologyStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New returned error: %v", err)
	}
	return &pgOntologyStore{db: db}, mock, func() { _ = db.Close() }
}

func TestPGOntologyStoreSaveAndGet(t *testing.T) {
	store, mock, closeDB := newMockOntologyDB(t)
	defer closeDB()

	ont := validTestOntology()
	ont.ID = "ont-1"
	ont.CreatedAt = time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectExec(regexp.QuoteMeta(sqlUpsertOntology)).
		WithArgs(ont.ID, ont.OrganizationID, sqlmock.AnyArg(), ont.CreatedAt, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.SaveOntology(context.Background(), ont); err != nil {
		t.Fatalf("SaveOntology returned error: %v", err)
	}
	if ont.UpdatedAt.IsZero() {
		t.Fatal("SaveOntology must stamp updated_at")
	}

	schema, err := json.Marshal(ont)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rows := sqlmock.NewRows([]string{"id", "organization_id", "schema", "created_at", "updated_at"}).
		AddRow(ont.ID, ont.OrganizationID, string(schema), ont.CreatedAt, ont.UpdatedAt)
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectOntology)).
		WithArgs(ont.ID, ont.OrganizationID).
		WillReturnRows(rows)
	got, err := store.GetOntology(context.Background(), ont.OrganizationID, ont.ID)
	if err != nil {
		t.Fatalf("GetOntology returned error: %v", err)
	}
	if got.ID != ont.ID || !reflect.DeepEqual(got.EntityTypes, ont.EntityTypes) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGOntologyStoreNotFoundAndList(t *testing.T) {
	store, mock, closeDB := newMockOntologyDB(t)
	defer closeDB()

	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectOntology)).
		WithArgs("missing", "org-1").
		WillReturnError(sql.ErrNoRows)
	if _, err := store.GetOntology(context.Background(), "org-1", "missing"); !errors.Is(err, ErrOntologyNotFound) {
		t.Fatalf("want ErrOntologyNotFound, got %v", err)
	}

	ont := validTestOntology()
	schema, _ := json.Marshal(ont)
	rows := sqlmock.NewRows([]string{"id", "organization_id", "schema", "created_at", "updated_at"}).
		AddRow("ont-1", "org-1", string(schema), time.Now(), time.Now())
	mock.ExpectQuery(regexp.QuoteMeta(sqlSelectOntologies)).
		WithArgs("org-1").
		WillReturnRows(rows)
	list, err := store.ListOntologies(context.Background(), "org-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListOntologies = %d (err %v), want 1", len(list), err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pending expectations: %v", err)
	}
}

func TestPGOntologyStoreGuard(t *testing.T) {
	var nilStore *pgOntologyStore
	if err := nilStore.guard(); err == nil {
		t.Fatal("nil db must be guarded")
	}
}
