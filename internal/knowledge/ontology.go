package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Ontology-first knowledge construction — issue #131.
//
// An Ontology is the schema of an organization's knowledge graph: typed
// entity types (PascalCase), typed edge types (UPPER_SNAKE_CASE), and
// snake_case attribute definitions. Extraction is SCHEMA-DRIVEN: candidate
// entities and attributes surfaced from text are validated against the
// schema and every type error is collected into ExtractionResult.Rejected —
// never dropped silently.
//
// Generation is seam-first: LLMGateway is the narrow interface for LLM-driven
// generation; RuleBasedGenerator is the deterministic default (frequency-
// based proper-noun/noun clustering, documented below) used whenever no LLM
// is wired, and as the fallback when generation must not depend on network
// availability. The guaranteed fallback types (Person, Organization) are
// ALWAYS present in every generated ontology, whatever the generator.
// ---------------------------------------------------------------------------

// AttributeType enumerates the value types of attribute definitions.
type AttributeType string

// Attribute value types (validated by ValidateOntology).
const (
	AttrTypeString   AttributeType = "string"
	AttrTypeNumber   AttributeType = "number"
	AttrTypeInteger  AttributeType = "integer"
	AttrTypeBoolean  AttributeType = "boolean"
	AttrTypeDatetime AttributeType = "datetime"
)

// ValidAttributeType reports whether t is one of the five attribute types.
func ValidAttributeType(t AttributeType) bool {
	switch t {
	case AttrTypeString, AttrTypeNumber, AttrTypeInteger, AttrTypeBoolean, AttrTypeDatetime:
		return true
	}
	return false
}

// AttributeDef defines one typed attribute of an entity or edge type. Name
// MUST be snake_case (enforced by ValidateOntology).
type AttributeDef struct {
	Name        string        `json:"name"`
	Type        AttributeType `json:"type"`
	Required    bool          `json:"required,omitempty"`
	Description string        `json:"description,omitempty"`
}

// EntityType is one node type of the knowledge graph. Name MUST be
// PascalCase. Fallback marks the guaranteed types (Person, Organization)
// that every ontology always carries. Cues are lowercase trigger words the
// deterministic extractor matches against a candidate's context (the
// preceding/following token or the candidate itself); the shipped fallback
// types carry builtin cues, custom types may declare their own.
type EntityType struct {
	Name       string         `json:"name"`
	Attributes []AttributeDef `json:"attributes,omitempty"`
	Fallback   bool           `json:"fallback,omitempty"`
	Cues       []string       `json:"cues,omitempty"`
}

// EdgeType is one relationship type of the knowledge graph. Name MUST be
// UPPER_SNAKE_CASE; SourceType/TargetType MUST reference declared entity
// type names (exact case). Multiple edge types may share a Name when they
// connect different (source, target) pairs; the pair triple is the identity.
type EdgeType struct {
	Name       string         `json:"name"`
	SourceType string         `json:"source_type"`
	TargetType string         `json:"target_type"`
	Attributes []AttributeDef `json:"attributes,omitempty"`
}

// Ontology is the org-scoped schema of one knowledge graph. AttributeDefs is
// the shared attribute library of the ontology (the canonical reserved
// attributes uuid/name/created_at ship by default); per-type attributes may
// extend it but never redefine a reserved name with a different type.
type Ontology struct {
	ID             string         `json:"id,omitempty"`
	OrganizationID string         `json:"organization_id,omitempty"`
	EntityTypes    []EntityType   `json:"entity_types"`
	EdgeTypes      []EdgeType     `json:"edge_types,omitempty"`
	AttributeDefs  []AttributeDef `json:"attribute_defs,omitempty"`
	CreatedAt      time.Time      `json:"created_at,omitempty"`
	UpdatedAt      time.Time      `json:"updated_at,omitempty"`
}

// Naming conventions (enforced by ValidateOntology on every generated or
// LLM-proposed schema; violations surface as typed validation errors, never
// silent renames).
var (
	pascalCaseRe     = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	upperSnakeCaseRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	snakeCaseRe      = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// IsPascalCase reports whether name follows the entity-type convention.
func IsPascalCase(name string) bool { return pascalCaseRe.MatchString(name) }

// IsUpperSnakeCase reports whether name follows the edge-type convention.
func IsUpperSnakeCase(name string) bool { return upperSnakeCaseRe.MatchString(name) }

// IsSnakeCase reports whether name follows the attribute convention.
func IsSnakeCase(name string) bool { return snakeCaseRe.MatchString(name) }

// Reserved attribute names: every entity type carries uuid, name and
// created_at. Redefining them with a different type is a validation error;
// the generators always inject the canonical definitions.
var reservedAttributeDefs = []AttributeDef{
	{Name: "uuid", Type: AttrTypeString, Required: true, Description: "stable entity identifier"},
	{Name: "name", Type: AttrTypeString, Required: true, Description: "human-readable surface name"},
	{Name: "created_at", Type: AttrTypeDatetime, Description: "entity creation time"},
}

// CanonicalAttributes returns a fresh copy of the reserved attribute
// definitions (callers may mutate their copy freely).
func CanonicalAttributes() []AttributeDef {
	return append([]AttributeDef(nil), reservedAttributeDefs...)
}

// fallbackTypeNames are the guaranteed entity types (GUARANTEE of issue
// #131): present in every generated ontology regardless of the corpus.
var fallbackTypeNames = []string{"Person", "Organization"}

// builtinCues maps the fallback types to their deterministic extraction cues
// (courtesy titles before a Person; org suffixes inside/after an
// Organization). Lowercase; matched case-insensitively.
var builtinCues = map[string][]string{
	"Person": {"mr", "mrs", "ms", "dr", "prof", "sir", "madam"},
	"Organization": {
		"inc", "corp", "corporation", "llc", "ltd", "gmbh", "plc", "co",
		"company", "university", "institute", "agency", "foundation", "labs",
	},
}

// ---------------------------------------------------------------------------
// Typed validation errors (collected, never fail-fast).
// ---------------------------------------------------------------------------

// ErrOntologyValidation is the sentinel wrapped by every ontology validation
// failure; use errors.Is to classify and type-assert to
// *OntologyValidationError for the field details.
var ErrOntologyValidation = errors.New("knowledge: ontology validation failed")

// OntologyFieldError pins one schema violation to its field path (e.g.
// "entity_types[2].name").
type OntologyFieldError struct {
	Field string `json:"field"`
	Issue string `json:"issue"`
}

// OntologyValidationError carries ALL field violations of one Validate run
// (collected, never silent — the caller sees the full schema diff at once).
type OntologyValidationError struct {
	Fields []OntologyFieldError `json:"fields"`
}

// Error implements error; it always wraps ErrOntologyValidation (Is).
func (e *OntologyValidationError) Error() string {
	var b strings.Builder
	b.WriteString(ErrOntologyValidation.Error())
	b.WriteString(fmt.Sprintf(" (%d issue(s))", len(e.Fields)))
	for _, f := range e.Fields {
		b.WriteString("; ")
		b.WriteString(f.Field)
		b.WriteString(": ")
		b.WriteString(f.Issue)
	}
	return b.String()
}

// Is reports compatibility with the ErrOntologyValidation sentinel.
func (e *OntologyValidationError) Is(target error) bool { return target == ErrOntologyValidation }

// ValidateOntology checks naming conventions, reserved attributes, attribute
// types and edge endpoint references. All violations are collected into one
// *OntologyValidationError (nil when the schema is valid).
func ValidateOntology(o *Ontology) error {
	var fields []OntologyFieldError
	add := func(field, issue string) {
		fields = append(fields, OntologyFieldError{Field: field, Issue: issue})
	}
	if o == nil {
		add("ontology", "ontology is nil")
		return &OntologyValidationError{Fields: fields}
	}

	seenEntity := make(map[string]bool, len(o.EntityTypes))
	entityNames := make(map[string]bool, len(o.EntityTypes))
	if len(o.EntityTypes) == 0 {
		add("entity_types", "at least one entity type is required")
	}
	for i, et := range o.EntityTypes {
		path := fmt.Sprintf("entity_types[%d]", i)
		if !IsPascalCase(et.Name) {
			add(path+".name", fmt.Sprintf("%q is not PascalCase", et.Name))
		}
		if seenEntity[strings.ToLower(et.Name)] {
			add(path+".name", fmt.Sprintf("duplicate entity type name %q", et.Name))
		}
		seenEntity[strings.ToLower(et.Name)] = true
		entityNames[et.Name] = true
		fields = append(fields, validateAttributes(path+".attributes", et.Attributes)...)
	}

	fields = append(fields, validateAttributes("attribute_defs", o.AttributeDefs)...)
	seenAttrDef := make(map[string]bool, len(o.AttributeDefs))
	for i, ad := range o.AttributeDefs {
		if seenAttrDef[ad.Name] {
			add(fmt.Sprintf("attribute_defs[%d].name", i), fmt.Sprintf("duplicate attribute definition %q", ad.Name))
		}
		seenAttrDef[ad.Name] = true
	}

	seenEdge := make(map[string]bool, len(o.EdgeTypes))
	for i, eg := range o.EdgeTypes {
		path := fmt.Sprintf("edge_types[%d]", i)
		if !IsUpperSnakeCase(eg.Name) {
			add(path+".name", fmt.Sprintf("%q is not UPPER_SNAKE_CASE", eg.Name))
		}
		if eg.SourceType == eg.TargetType {
			add(path, fmt.Sprintf("edge %q connects %q to itself", eg.Name, eg.SourceType))
		}
		if !entityNames[eg.SourceType] {
			add(path+".source_type", fmt.Sprintf("unknown entity type %q", eg.SourceType))
		}
		if !entityNames[eg.TargetType] {
			add(path+".target_type", fmt.Sprintf("unknown entity type %q", eg.TargetType))
		}
		key := eg.Name + "\x1f" + eg.SourceType + "\x1f" + eg.TargetType
		if seenEdge[key] {
			add(path, fmt.Sprintf("duplicate edge %q (%s -> %s)", eg.Name, eg.SourceType, eg.TargetType))
		}
		seenEdge[key] = true
		fields = append(fields, validateAttributes(path+".attributes", eg.Attributes)...)
	}

	if len(fields) == 0 {
		return nil
	}
	return &OntologyValidationError{Fields: fields}
}

// validateAttributes checks snake_case names, type enums, duplicates and the
// reserved-attribute redefinition rule for one attribute list.
func validateAttributes(path string, attrs []AttributeDef) []OntologyFieldError {
	var fields []OntologyFieldError
	seen := make(map[string]bool, len(attrs))
	for i, ad := range attrs {
		attrPath := fmt.Sprintf("%s[%d]", path, i)
		lower := strings.ToLower(ad.Name)
		if !IsSnakeCase(ad.Name) {
			fields = append(fields, OntologyFieldError{
				Field: attrPath + ".name",
				Issue: fmt.Sprintf("%q is not snake_case", ad.Name),
			})
		}
		if !ValidAttributeType(ad.Type) {
			fields = append(fields, OntologyFieldError{
				Field: attrPath + ".type",
				Issue: fmt.Sprintf("%q is not a valid attribute type (string|number|integer|boolean|datetime)", ad.Type),
			})
		}
		if seen[lower] {
			fields = append(fields, OntologyFieldError{
				Field: attrPath + ".name",
				Issue: fmt.Sprintf("duplicate attribute %q", ad.Name),
			})
		}
		seen[lower] = true
		for _, res := range reservedAttributeDefs {
			if ad.Name == res.Name && ad.Type != res.Type {
				fields = append(fields, OntologyFieldError{
					Field: attrPath + ".type",
					Issue: fmt.Sprintf("reserved attribute %q must keep type %q (got %q)", res.Name, res.Type, ad.Type),
				})
			}
		}
	}
	return fields
}

// ---------------------------------------------------------------------------
// Generation: LLMGateway seam + deterministic RuleBasedGenerator default.
// ---------------------------------------------------------------------------

// GenerateOptions tunes ontology generation.
type GenerateOptions struct {
	// MaxEntityTypes caps the GENERATED types (the two guaranteed fallback
	// types are always added on top). Default DefaultOntologyEntityTypes.
	MaxEntityTypes int
	// MaxEdgeTypes caps generated edge types. Default equals MaxEntityTypes.
	MaxEdgeTypes int
	// Window is the co-occurrence token window for edge generation
	// (default DefaultOntologyEdgeWindow).
	Window int
}

// Generation defaults.
const (
	// DefaultOntologyEntityTypes is the configurable N of issue #131.
	DefaultOntologyEntityTypes = 10
	// DefaultOntologyEdgeWindow is the token window in which two entity
	// mentions co-occur for edge generation/extraction.
	DefaultOntologyEdgeWindow = 12
	// llmPromptCorpusRunes caps the corpus embedded into the LLM prompt.
	llmPromptCorpusRunes = 6000
)

// OntologyGenerator produces an org's ontology from document text.
// Implementations: RuleBasedGenerator (deterministic default) and
// LLMOntologyGenerator (LLMGateway seam).
type OntologyGenerator interface {
	GenerateOntology(ctx context.Context, corpus string, opts GenerateOptions) (*Ontology, error)
}

// LLMGateway is the NARROW seam for LLM-driven ontology generation: one
// prompt in, one completion out. Wiring an actual model client is an
// infrastructure concern — adapters implement this interface over any
// OpenAI-compatible chat endpoint without this package importing model
// SDKs (same pattern as the Embedder seam).
type LLMGateway interface {
	// Generate returns the model completion for the prompt (expected to be
	// strict JSON per the prompt contract of LLMOntologyGenerator).
	Generate(ctx context.Context, prompt string) (string, error)
}

// Compile-time proofs the generators satisfy the OntologyGenerator seam.
var (
	_ OntologyGenerator = (*RuleBasedGenerator)(nil)
	_ OntologyGenerator = (*LLMOntologyGenerator)(nil)
)

// RuleBasedGenerator is the deterministic default OntologyGenerator.
//
// Documented algorithm (frequency-based noun / proper-noun clustering):
//
//  1. Tokenize the corpus preserving case.
//  2. PROPER-NOUN candidates: tokens whose first rune is uppercase and that
//     are not stopwords (e.g. sentence-initial "The"). These rank first.
//  3. COMMON-NOUN candidates: lowercase tokens of >= 4 runes that are not
//     stopwords; they fill remaining slots when proper nouns are scarce.
//     No stemming is applied (documented: surface forms are kept verbatim).
//  4. Rank by frequency (desc), tie-break alphabetically (asc) — fully
//     deterministic for identical corpora. The top N (GenerateOptions.
//     MaxEntityTypes, default 10) become entity types, PascalCase-normalized.
//  5. EDGE types: pairs of generated entity tokens co-occurring within the
//     token window (default 12) are counted (unordered, canonically ordered
//     alphabetically); the top MaxEdgeTypes pairs become RELATES_TO edge
//     types between the PascalCase pair (distinct (source,target) triples
//     may share the RELATES_TO name — the triple is the edge identity).
//  6. Every entity type receives the canonical reserved attributes
//     (uuid, name, created_at); the guaranteed fallback types (Person,
//     Organization) are ALWAYS appended with their builtin cues.
type RuleBasedGenerator struct{}

// NewRuleBasedGenerator returns the deterministic default generator.
func NewRuleBasedGenerator() *RuleBasedGenerator { return &RuleBasedGenerator{} }

// GenerateOntology implements OntologyGenerator. It never contacts the
// network and returns the same ontology for the same corpus (test:
// TestRuleBasedGeneratorDeterministic).
func (g RuleBasedGenerator) GenerateOntology(_ context.Context, corpus string, opts GenerateOptions) (*Ontology, error) {
	if opts.MaxEntityTypes <= 0 {
		opts.MaxEntityTypes = DefaultOntologyEntityTypes
	}
	if opts.MaxEdgeTypes <= 0 {
		opts.MaxEdgeTypes = opts.MaxEntityTypes
	}
	if opts.Window <= 0 {
		opts.Window = DefaultOntologyEdgeWindow
	}

	tokens := caseTokens(corpus)
	proper, common := countNounCandidates(tokens)

	// Rank candidates: frequency desc, then alphabetical asc.
	rank := func(counts map[string]int) []string {
		out := make([]string, 0, len(counts))
		for tok := range counts {
			out = append(out, tok)
		}
		sort.Slice(out, func(i, j int) bool {
			if counts[out[i]] != counts[out[j]] {
				return counts[out[i]] > counts[out[j]]
			}
			return out[i] < out[j]
		})
		return out
	}

	picked := make([]string, 0, opts.MaxEntityTypes)
	used := make(map[string]bool)
	for _, tok := range rank(proper) {
		if len(picked) >= opts.MaxEntityTypes {
			break
		}
		name := pascalize(tok)
		if used[strings.ToLower(name)] {
			continue
		}
		used[strings.ToLower(name)] = true
		picked = append(picked, tok)
	}
	if len(picked) < opts.MaxEntityTypes {
		for _, tok := range rank(common) {
			if len(picked) >= opts.MaxEntityTypes {
				break
			}
			name := pascalize(tok)
			if used[strings.ToLower(name)] {
				continue
			}
			used[strings.ToLower(name)] = true
			picked = append(picked, tok)
		}
	}

	ont := &Ontology{AttributeDefs: CanonicalAttributes()}
	for _, tok := range picked {
		ont.EntityTypes = append(ont.EntityTypes, EntityType{
			Name:       pascalize(tok),
			Attributes: CanonicalAttributes(),
		})
	}
	ont.EdgeTypes = ruleBasedEdges(tokens, picked, opts.MaxEdgeTypes, opts.Window)
	ensureFallbackTypes(ont)
	if err := ValidateOntology(ont); err != nil {
		return nil, err // unreachable for generated schemas; kept honest
	}
	return ont, nil
}

// ruleBasedEdges counts co-occurring entity-token pairs inside the window
// and returns the top ones as RELATES_TO edge types (pair order canonical:
// alphabetical by token).
func ruleBasedEdges(tokens []string, entityTokens []string, maxEdges, window int) []EdgeType {
	wanted := make(map[string]string, len(entityTokens)) // lower -> pascal
	for _, tok := range entityTokens {
		wanted[strings.ToLower(tok)] = pascalize(tok)
	}
	counts := make(map[[2]string]int)
	if window < 1 {
		window = 1
	}
	for i := 0; i < len(tokens); i++ {
		a, okA := wanted[strings.ToLower(tokens[i])]
		if !okA {
			continue
		}
		for j := i + 1; j < len(tokens) && j <= i+window; j++ {
			b, okB := wanted[strings.ToLower(tokens[j])]
			if !okB || b == a {
				continue
			}
			pair := [2]string{a, b}
			if pair[0] > pair[1] {
				pair = [2]string{b, a}
			}
			counts[pair]++
		}
	}
	type pairCount struct {
		pair  [2]string
		count int
	}
	pairs := make([]pairCount, 0, len(counts))
	for p, c := range counts {
		pairs = append(pairs, pairCount{pair: p, count: c})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		if pairs[i].pair[0] != pairs[j].pair[0] {
			return pairs[i].pair[0] < pairs[j].pair[0]
		}
		return pairs[i].pair[1] < pairs[j].pair[1]
	})
	if len(pairs) > maxEdges {
		pairs = pairs[:maxEdges]
	}
	edges := make([]EdgeType, 0, len(pairs))
	for _, p := range pairs {
		edges = append(edges, EdgeType{Name: "RELATES_TO", SourceType: p.pair[0], TargetType: p.pair[1]})
	}
	return edges
}

// ensureFallbackTypes GUARANTEES the fallback types: Person and Organization
// are appended (with canonical attributes + builtin cues) when missing, and
// forced Fallback=true when present. This runs on EVERY generated or
// LLM-proposed ontology.
func ensureFallbackTypes(ont *Ontology) {
	for _, name := range fallbackTypeNames {
		found := false
		for i := range ont.EntityTypes {
			if strings.EqualFold(ont.EntityTypes[i].Name, name) {
				ont.EntityTypes[i].Fallback = true
				ont.EntityTypes[i].Attributes = mergeCanonicalAttributes(ont.EntityTypes[i].Attributes)
				ont.EntityTypes[i].Cues = mergeCues(ont.EntityTypes[i].Cues, builtinCues[name])
				found = true
			}
		}
		if found {
			continue
		}
		ont.EntityTypes = append(ont.EntityTypes, EntityType{
			Name:       name,
			Attributes: CanonicalAttributes(),
			Fallback:   true,
			Cues:       append([]string(nil), builtinCues[name]...),
		})
	}
}

// mergeCanonicalAttributes injects any missing reserved attribute into attrs.
func mergeCanonicalAttributes(attrs []AttributeDef) []AttributeDef {
	out := append([]AttributeDef(nil), attrs...)
	for _, res := range reservedAttributeDefs {
		present := false
		for _, a := range out {
			if a.Name == res.Name {
				present = true
				break
			}
		}
		if !present {
			out = append(out, res)
		}
	}
	return out
}

func mergeCues(cues, extra []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(cues)+len(extra))
	for _, c := range append(append([]string(nil), cues...), extra...) {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// ---------------------------------------------------------------------------
// LLM-driven generation over the narrow gateway seam.
// ---------------------------------------------------------------------------

// LLMOntologyGenerator generates an ontology with an LLM behind the
// LLMGateway seam. The prompt is deterministic (same corpus -> same prompt);
// the completion is parsed as JSON into the Ontology wire shape, then passed
// through the SAME guarantees as the rule-based path: fallback types and
// canonical reserved attributes are ensured additively, and the schema is
// validated — violations surface as *OntologyValidationError (never silent
// fixes or renames).
type LLMOntologyGenerator struct {
	Gateway LLMGateway
}

// NewLLMOntologyGenerator returns a generator over the given gateway.
func NewLLMOntologyGenerator(gateway LLMGateway) *LLMOntologyGenerator {
	return &LLMOntologyGenerator{Gateway: gateway}
}

// GenerateOntology implements OntologyGenerator.
func (g *LLMOntologyGenerator) GenerateOntology(ctx context.Context, corpus string, opts GenerateOptions) (*Ontology, error) {
	if g == nil || g.Gateway == nil {
		return nil, errors.New("knowledge: LLM gateway is required")
	}
	if opts.MaxEntityTypes <= 0 {
		opts.MaxEntityTypes = DefaultOntologyEntityTypes
	}
	prompt := llmOntologyPrompt(corpus, opts.MaxEntityTypes)
	raw, err := g.Gateway.Generate(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("knowledge: ontology LLM generation failed: %w", err)
	}
	ont, err := parseOntologyJSON(raw)
	if err != nil {
		return nil, err
	}
	ensureFallbackTypes(ont)
	if err := ValidateOntology(ont); err != nil {
		return nil, err
	}
	return ont, nil
}

// llmOntologyPrompt builds the deterministic generation prompt (corpus
// capped at llmPromptCorpusRunes runes).
func llmOntologyPrompt(corpus string, maxEntityTypes int) string {
	runes := []rune(strings.TrimSpace(corpus))
	if len(runes) > llmPromptCorpusRunes {
		runes = runes[:llmPromptCorpusRunes]
	}
	var b strings.Builder
	b.WriteString("You are an ontology engineer. From the DOCUMENT below, propose a knowledge-graph ontology.\n")
	b.WriteString("Reply with STRICT JSON only, no prose, shape:\n")
	b.WriteString(`{"entity_types":[{"name":"PascalCase","attributes":[{"name":"snake_case","type":"string|number|integer|boolean|datetime","required":true}]}],"edge_types":[{"name":"UPPER_SNAKE_CASE","source_type":"PascalCase","target_type":"PascalCase","attributes":[]}]}` + "\n")
	b.WriteString("Rules: at most " + strconv.Itoa(maxEntityTypes) + " entity types; entity names PascalCase; edge names UPPER_SNAKE_CASE; attribute names snake_case; every entity type defines attributes uuid (string), name (string), created_at (datetime); never redefine a reserved attribute with another type; edge source_type/target_type must name declared entity types.\n")
	b.WriteString("DOCUMENT:\n")
	b.WriteString(string(runes))
	return b.String()
}

// parseOntologyJSON decodes the LLM completion into the Ontology wire shape.
func parseOntologyJSON(raw string) (*Ontology, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)
	var ont Ontology
	if err := json.Unmarshal([]byte(trimmed), &ont); err != nil {
		return nil, fmt.Errorf("knowledge: ontology LLM returned invalid JSON: %w", err)
	}
	return &ont, nil
}

// ---------------------------------------------------------------------------
// Schema-driven extraction with type validation.
// ---------------------------------------------------------------------------

// ExtractionResult is the outcome of one extraction query: typed entities,
// co-occurrence edges, and EVERY schema violation collected in Rejected
// (type errors are never silent).
type ExtractionResult struct {
	Entities []ExtractedEntity `json:"entities"`
	Edges    []ExtractedEdge   `json:"edges,omitempty"`
	Rejected []ExtractionError `json:"rejected,omitempty"`
}

// ExtractedEntity is one typed entity mention (deduplicated by type+name).
type ExtractedEntity struct {
	Type       string         `json:"type"`
	Name       string         `json:"name"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Position   int            `json:"position,omitempty"`
}

// ExtractedEdge is one typed relationship between two extracted entities.
type ExtractedEdge struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	Target string `json:"target"`
}

// ExtractionError records one candidate that failed schema validation.
type ExtractionError struct {
	Surface string `json:"surface"`
	Type    string `json:"type,omitempty"`
	Reason  string `json:"reason"`
}

// attrValueWindow is how many tokens after an entity mention an
// `attribute: value` pair may appear and still attach to it.
const attrValueWindow = 8

// ExtractEntities runs schema-driven extraction: capitalized candidate
// sequences are typed against the ontology (cue words in the candidate or
// its adjacent tokens, or an exact name echo of the type itself), attribute
// values are parsed per the declared AttributeType, and co-occurring typed
// entities are connected with matching declared edge types. Candidates no
// entity type claims are dropped (documented: unclaimed candidates are not
// schema violations); SCHEMA violations — invalid ontology, undeclared or
// mistyped attribute values — are collected in Rejected, never silent.
func ExtractEntities(text string, ont *Ontology) *ExtractionResult {
	res := &ExtractionResult{Entities: []ExtractedEntity{}}
	if err := ValidateOntology(ont); err != nil {
		res.Rejected = append(res.Rejected, ExtractionError{Surface: "ontology", Reason: err.Error()})
		return res
	}
	tokens := caseTokens(text)

	// 1. Candidate runs: maximal sequences of capitalized, non-stopword
	// tokens. Stopword tokens break runs (so sentence starters and courtesy
	// titles never become entity surface forms).
	type candidate struct {
		name  string
		start int
		end   int // inclusive token index of the last run token
	}
	var candidates []candidate
	for i := 0; i < len(tokens); {
		if !startsWithUpper(tokens[i]) || isStopword(tokens[i]) {
			i++
			continue
		}
		j := i
		for j+1 < len(tokens) && startsWithUpper(tokens[j+1]) && !isStopword(tokens[j+1]) {
			j++
		}
		candidates = append(candidates, candidate{name: strings.Join(tokens[i:j+1], " "), start: i, end: j})
		i = j + 1
	}

	// 2. Type each candidate (ontology order wins; deterministic).
	typed := make([]ExtractedEntity, 0, len(candidates))
	seen := make(map[string]bool)
	for _, cand := range candidates {
		et, ok := matchEntityType(cand.name, cand.start, cand.end, tokens, ont)
		if !ok {
			continue
		}
		key := et + "\x1f" + strings.ToLower(cand.name)
		if seen[key] {
			continue
		}
		seen[key] = true
		typed = append(typed, ExtractedEntity{Type: et, Name: cand.name, Position: cand.start})
	}

	// 3. Attributes: `attr: value` / `attr = value` pairs for declared
	// attributes, attached to the nearest preceding mention of that type
	// within attrValueWindow tokens. Mismatches are collected, never silent.
	typed = extractAttributes(text, tokens, ont, typed, res)

	// 4. Edges: co-occurrence within DefaultOntologyEdgeWindow tokens,
	// connected only through DECLARED edge types (orientation respected).
	res.Edges = extractEdges(typed, ont)
	res.Entities = typed
	return res
}

// QueryEntities is the type-filtered extraction query: it runs
// ExtractEntities and keeps only entities whose type is in the requested
// set (plus edges whose endpoints survive the filter). Requesting a type
// the ontology does not declare is itself collected as a Rejected entry —
// type-based filtering is validated like everything else.
func QueryEntities(text string, ont *Ontology, types ...string) *ExtractionResult {
	res := ExtractEntities(text, ont)
	if ont == nil {
		return res
	}
	declared := make(map[string]bool, len(ont.EntityTypes))
	for _, et := range ont.EntityTypes {
		declared[et.Name] = true
	}
	wanted := make(map[string]bool, len(types))
	for _, t := range types {
		if !declared[t] {
			res.Rejected = append(res.Rejected, ExtractionError{
				Surface: t,
				Type:    t,
				Reason:  "type filter references an entity type not declared in the ontology",
			})
			continue
		}
		wanted[t] = true
	}
	if len(wanted) == 0 && len(types) > 0 {
		res.Entities = []ExtractedEntity{}
		res.Edges = nil
		return res
	}
	if len(types) == 0 {
		return res // no filter requested
	}
	filtered := make([]ExtractedEntity, 0, len(res.Entities))
	kept := make(map[string]bool)
	for _, e := range res.Entities {
		if wanted[e.Type] {
			filtered = append(filtered, e)
			kept[e.Name] = true
		}
	}
	res.Entities = filtered
	edges := make([]ExtractedEdge, 0, len(res.Edges))
	for _, e := range res.Edges {
		if kept[e.Source] && kept[e.Target] {
			edges = append(edges, e)
		}
	}
	res.Edges = edges
	return res
}

// matchEntityType types one candidate: exact name echo of the type, or a cue
// match on the candidate itself, its preceding token, or its following token.
func matchEntityType(name string, start, end int, tokens []string, ont *Ontology) (string, bool) {
	lowerName := strings.ToLower(name)
	lowerTokens := make([]string, len(tokens))
	for i, tok := range tokens {
		lowerTokens[i] = strings.ToLower(tok)
	}
	for _, et := range ont.EntityTypes {
		if strings.EqualFold(et.Name, name) {
			return et.Name, true
		}
		matched := false
		for _, cue := range et.Cues {
			cue = strings.ToLower(cue)
			if cue == "" {
				continue
			}
			if strings.Contains(lowerName, cue) {
				matched = true
				break
			}
			if start > 0 && lowerTokens[start-1] == cue {
				matched = true
				break
			}
			if end+1 < len(lowerTokens) && lowerTokens[end+1] == cue {
				matched = true
				break
			}
		}
		if matched {
			return et.Name, true
		}
	}
	return "", false
}

// attributeValueRe builds the per-attribute value pattern: `attr: value` or
// `attr = value`, value = quoted string or a run of non-separator chars
// (keeps datetimes with colons intact).
func attributeValueRe(attr string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(attr) + `\s*[:=]\s*("[^"]*"|[^.,;!\s]+)`)
}

// attrTokenTypes maps the declared attribute type onto its Go-side parse.
var attrTokenParsers = map[AttributeType]func(string) (any, bool){
	AttrTypeString:   func(v string) (any, bool) { return v, v != "" },
	AttrTypeNumber:   func(v string) (any, bool) { f, err := strconv.ParseFloat(v, 64); return f, err == nil },
	AttrTypeInteger:  func(v string) (any, bool) { n, err := strconv.ParseInt(v, 10, 64); return n, err == nil },
	AttrTypeBoolean:  parseBooleanValue,
	AttrTypeDatetime: parseDatetimeValue,
}

// parseBooleanValue accepts only the exact literals true/false.
func parseBooleanValue(v string) (any, bool) {
	switch strings.ToLower(v) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return nil, false
}

func parseDatetimeValue(v string) (any, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if ts, err := time.Parse(layout, v); err == nil {
			return ts.Format(time.RFC3339), true
		}
	}
	return nil, false
}

// extractAttributes scans `attr: value` pairs for every declared attribute
// of every matched entity type, validates the value against the declared
// type and attaches it to the nearest preceding mention of that type within
// attrValueWindow tokens. Value type errors are collected into res.Rejected.
func extractAttributes(text string, tokens []string, ont *Ontology, entities []ExtractedEntity, res *ExtractionResult) []ExtractedEntity {
	// tokenIndexAt maps byte offsets onto token ordinals (single pass).
	tokenStarts := make([]int, 0, len(tokens))
	{
		pos := 0
		for _, tok := range tokens {
			idx := strings.Index(text[pos:], tok)
			if idx < 0 {
				idx = 0
			}
			at := pos + idx
			tokenStarts = append(tokenStarts, at)
			pos = at + len(tok)
		}
	}
	tokenAt := func(offset int) int {
		lo, hi := 0, len(tokenStarts)-1
		at := -1
		for lo <= hi {
			mid := (lo + hi) / 2
			if tokenStarts[mid] <= offset {
				at = mid
				lo = mid + 1
			} else {
				hi = mid - 1
			}
		}
		return at
	}

	attributesByType := make(map[string][]AttributeDef, len(ont.EntityTypes))
	for _, et := range ont.EntityTypes {
		attributesByType[et.Name] = et.Attributes
	}

	for _, et := range ont.EntityTypes {
		declared := make(map[string]AttributeDef, len(et.Attributes))
		for _, ad := range et.Attributes {
			declared[ad.Name] = ad
		}
		for attrName, ad := range declared {
			if ad.Name == "uuid" || ad.Name == "name" || ad.Name == "created_at" {
				// Reserved attributes are system-assigned at materialization,
				// never parsed from text.
				continue
			}
			for _, m := range attributeValueRe(attrName).FindAllStringSubmatchIndex(text, -1) {
				value := strings.Trim(text[m[2]:m[3]], `"`)
				raw := value
				parse, ok := attrTokenParsers[ad.Type]
				if !ok {
					continue
				}
				converted, ok := parse(raw)
				at := tokenAt(m[2])
				if !ok {
					res.Rejected = append(res.Rejected, ExtractionError{
						Surface: raw,
						Type:    et.Name,
						Reason:  fmt.Sprintf("attribute %q expects %s, got %q", ad.Name, ad.Type, raw),
					})
					continue
				}
				// Nearest preceding mention of this type within the window.
				best := -1
				for i := range entities {
					e := &entities[i]
					if e.Type != et.Name || e.Position > at || at-e.Position > attrValueWindow {
						continue
					}
					if best == -1 || entities[i].Position > entities[best].Position {
						best = i
					}
				}
				if best == -1 {
					continue // no nearby mention: not evidence, not an error
				}
				if entities[best].Attributes == nil {
					entities[best].Attributes = make(map[string]any)
				}
				entities[best].Attributes[ad.Name] = converted
			}
		}
	}
	return entities
}

// extractEdges connects co-occurring typed entities through DECLARED edge
// types only. Both orientations are tried (a declared edge A -> B fires for
// mentions (x@A, y@B) in either textual order). Results are deduplicated by
// (type, source, target) and ordered by first occurrence.
func extractEdges(entities []ExtractedEntity, ont *Ontology) []ExtractedEdge {
	if len(entities) < 2 || len(ont.EdgeTypes) == 0 {
		return nil
	}
	var out []ExtractedEdge
	seen := make(map[string]bool)
	for i := 0; i < len(entities); i++ {
		for j := i + 1; j < len(entities); j++ {
			dist := entities[j].Position - entities[i].Position
			if dist > DefaultOntologyEdgeWindow {
				break
			}
			a, b := entities[i], entities[j]
			for _, orient := range [][2]ExtractedEntity{{a, b}, {b, a}} {
				src, dst := orient[0], orient[1]
				for _, eg := range ont.EdgeTypes {
					if eg.SourceType != src.Type || eg.TargetType != dst.Type {
						continue
					}
					key := eg.Name + "\x1f" + src.Name + "\x1f" + dst.Name
					if !seen[key] {
						seen[key] = true
						out = append(out, ExtractedEdge{Type: eg.Name, Source: src.Name, Target: dst.Name})
					}
					break // first declared edge matching the orientation wins
				}
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Candidate counting helpers (shared by the rule-based generator).
// ---------------------------------------------------------------------------

// stopwordSet backs isStopword. Lowercase; matched case-insensitively so
// sentence-initial capitals and courtesy titles never surface as entities.
var stopwordSet = func() map[string]bool {
	words := []string{
		"a", "an", "the", "and", "but", "or", "if", "then", "else", "when",
		"while", "for", "with", "from", "into", "onto", "over", "under",
		"after", "before", "between", "during", "about", "against", "of",
		"to", "in", "on", "at", "by", "as", "is", "are", "was", "were", "be",
		"been", "being", "do", "does", "did", "has", "have", "had", "will",
		"would", "can", "could", "should", "shall", "may", "might", "must",
		"this", "that", "these", "those", "there", "here", "it", "its",
		"we", "our", "you", "your", "they", "them", "their", "she", "he",
		"his", "her", "all", "any", "each", "both", "few", "more", "most",
		"other", "some", "such", "only", "own", "same", "so", "than", "too",
		"very", "one", "two", "three", "new", "first", "last", "next",
		"mr", "mrs", "ms", "dr", "prof", "sir", "madam", "inc", "corp",
		"llc", "ltd", "gmbh", "plc", "co",
	}
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	return set
}()

func isStopword(token string) bool { return stopwordSet[strings.ToLower(token)] }

// startsWithUpper reports whether the token's first rune is uppercase.
func startsWithUpper(token string) bool {
	for _, r := range token {
		return unicode.IsUpper(r)
	}
	return false
}

// caseTokens splits text on non-letter/digit runes WITHOUT lowercasing
// (contrast tokenize() in scoring.go, which lowercases).
func caseTokens(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// countNounCandidates classifies tokens into proper-noun and common-noun
// frequency maps (documented RuleBasedGenerator algorithm steps 2-3).
func countNounCandidates(tokens []string) (proper, common map[string]int) {
	proper = make(map[string]int)
	common = make(map[string]int)
	for _, tok := range tokens {
		if len(tok) < 2 || isStopword(tok) {
			continue
		}
		switch {
		case startsWithUpper(tok):
			proper[tok]++
		case len(tok) >= 4:
			lower := strings.ToLower(tok)
			if !isStopword(lower) {
				common[lower]++
			}
		}
	}
	return proper, common
}

// pascalize normalizes a token to PascalCase (entity naming convention):
// the first rune is uppercased, the rest kept verbatim (no stemming).
func pascalize(token string) string {
	runes := []rune(strings.TrimSpace(token))
	if len(runes) == 0 {
		return ""
	}
	upper := unicode.ToUpper(runes[0])
	return string(upper) + string(runes[1:])
}

// ---------------------------------------------------------------------------
// Ontology persistence (org-scoped, JSONB schema — migration 035).
// ---------------------------------------------------------------------------

// ErrOntologyNotFound is returned for unknown or foreign ontology ids
// (tenant guard: foreign ids are "not found").
var ErrOntologyNotFound = errors.New("knowledge: ontology not found")

// OntologyStore abstracts durable ontology storage. Implementations MUST
// scope every query by organization_id.
type OntologyStore interface {
	// SaveOntology upserts by id (org carried in the row).
	SaveOntology(ctx context.Context, ont *Ontology) error
	// GetOntology returns one ontology of one tenant (ErrOntologyNotFound
	// for unknown ids or foreign tenants).
	GetOntology(ctx context.Context, orgID, id string) (*Ontology, error)
	// ListOntologies returns the org's ontologies, newest first.
	ListOntologies(ctx context.Context, orgID string) ([]*Ontology, error)
}

// SQL for ontology persistence. Tenant guard: organization_id on every
// statement. The whole schema (entity_types/edge_types/attribute_defs)
// lives in the JSONB column — the row shape is stable while the schema
// vocabulary evolves.
const (
	sqlUpsertOntology = `INSERT INTO knowledge_ontologies (id, organization_id, schema, created_at, updated_at) VALUES ($1, $2, $3::jsonb, $4, $5) ON CONFLICT (id) DO UPDATE SET schema = EXCLUDED.schema, updated_at = EXCLUDED.updated_at`
	// Tenant guard: org-scoped listing.
	sqlSelectOntologies = `SELECT id, organization_id, COALESCE(schema::text, '{}'), created_at, updated_at FROM knowledge_ontologies WHERE organization_id = $1 ORDER BY created_at DESC, id ASC`
	// Tenant guard: single read scoped to one organization_id.
	sqlSelectOntology = `SELECT id, organization_id, COALESCE(schema::text, '{}'), created_at, updated_at FROM knowledge_ontologies WHERE id = $1 AND organization_id = $2`
)

// pgOntologyStore is the Postgres-backed OntologyStore (migration 035).
type pgOntologyStore struct {
	db *sql.DB
}

// NewPostgresOntologyStore returns an OntologyStore backed by *sql.DB.
func NewPostgresOntologyStore(db *sql.DB) OntologyStore {
	return &pgOntologyStore{db: db}
}

func (s *pgOntologyStore) guard() error {
	if s == nil || s.db == nil {
		return errors.New("knowledge: ontology database is nil")
	}
	return nil
}

// SaveOntology upserts the ontology row (schema JSONB). An empty ID gets a
// generated UUID; timestamps are stamped when zero.
func (s *pgOntologyStore) SaveOntology(ctx context.Context, ont *Ontology) error {
	if err := s.guard(); err != nil {
		return err
	}
	if ont == nil {
		return errors.New("knowledge: ontology is nil")
	}
	if strings.TrimSpace(ont.ID) == "" {
		ont.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if ont.CreatedAt.IsZero() {
		ont.CreatedAt = now
	}
	ont.UpdatedAt = now
	schema, err := json.Marshal(ont)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, sqlUpsertOntology, ont.ID, ont.OrganizationID, string(schema), ont.CreatedAt, ont.UpdatedAt)
	return err
}

// GetOntology returns one org-scoped ontology (ErrOntologyNotFound on
// unknown id or foreign tenant).
func (s *pgOntologyStore) GetOntology(ctx context.Context, orgID, id string) (*Ontology, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, sqlSelectOntology, id, orgID)
	ont, err := scanOntology(row)
	if err != nil {
		return nil, err
	}
	return ont, nil
}

// ListOntologies returns the org's ontologies, newest first.
func (s *pgOntologyStore) ListOntologies(ctx context.Context, orgID string) ([]*Ontology, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqlSelectOntologies, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Ontology, 0)
	for rows.Next() {
		ont, err := scanOntology(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ont)
	}
	return out, rows.Err()
}

// scanner abstracts *sql.Rows and *sql.Row for the ontology scan helper.
type ontologyScanner interface {
	Scan(dest ...any) error
}

func scanOntology(sc ontologyScanner) (*Ontology, error) {
	var ont Ontology
	var schemaRaw string
	if err := sc.Scan(&ont.ID, &ont.OrganizationID, &schemaRaw, &ont.CreatedAt, &ont.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOntologyNotFound
		}
		return nil, err
	}
	if strings.TrimSpace(schemaRaw) != "" {
		var payload Ontology
		if err := json.Unmarshal([]byte(schemaRaw), &payload); err != nil {
			return nil, fmt.Errorf("knowledge: ontology schema decode: %w", err)
		}
		// Row identity (id/org/timestamps) wins over the JSON payload; the
		// payload only carries the schema vocabulary.
		ont.EntityTypes = payload.EntityTypes
		ont.EdgeTypes = payload.EdgeTypes
		ont.AttributeDefs = payload.AttributeDefs
	}
	return &ont, nil
}

// memoryOntologyStore is the mutex-guarded in-memory OntologyStore.
type memoryOntologyStore struct {
	mu   sync.Mutex
	byID map[string]Ontology
}

// NewInMemoryOntologyStore returns the in-memory OntologyStore.
func NewInMemoryOntologyStore() OntologyStore {
	return &memoryOntologyStore{byID: make(map[string]Ontology)}
}

// SaveOntology upserts a copy keyed by id.
func (m *memoryOntologyStore) SaveOntology(_ context.Context, ont *Ontology) error {
	if ont == nil {
		return errors.New("knowledge: ontology is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	if strings.TrimSpace(ont.ID) == "" {
		ont.ID = uuid.NewString()
	}
	if ont.CreatedAt.IsZero() {
		ont.CreatedAt = now
	}
	ont.UpdatedAt = now
	m.byID[ont.ID] = *ont
	return nil
}

// GetOntology returns a copy scoped to one tenant.
func (m *memoryOntologyStore) GetOntology(_ context.Context, orgID, id string) (*Ontology, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ont, ok := m.byID[id]
	if !ok || ont.OrganizationID != orgID {
		return nil, ErrOntologyNotFound
	}
	return &ont, nil
}

// ListOntologies returns the org's ontologies newest first (ties by id).
func (m *memoryOntologyStore) ListOntologies(_ context.Context, orgID string) ([]*Ontology, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Ontology, 0)
	for _, ont := range m.byID {
		if ont.OrganizationID != orgID {
			continue
		}
		cp := ont
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
