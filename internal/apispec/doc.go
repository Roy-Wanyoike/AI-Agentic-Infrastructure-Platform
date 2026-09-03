// Package apispec holds CI-time integrity checks for api/openapi.yaml.
//
// It is a TEST-ONLY package: it contains no production code and nothing here
// is linked into any binary. The tests parse the OpenAPI document and fail CI
// when the contract regresses (issue #17):
//
//   - every $ref resolves to an existing node in the document
//     (JSON pointers like #/components/schemas/Agent);
//   - operationIds are unique across the whole document;
//   - every path item declares at least one operation;
//   - every operation declares responses.
//
// The wave-3 integration had to repair 13 dangling refs after merging
// fragments into the 61-path / 118-schema document; these tests pin the
// contract so dangling refs and duplicate ids can no longer land. The OpenAPI
// document itself is READ-ONLY for this package: the tests are pure consumers
// of the contract.
package apispec
