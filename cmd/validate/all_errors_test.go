package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"
)

const twoErrorsSpec = `
openapi: 3.1.0
info: {title: t, version: "1"}
paths:
  /a:
    post:
      requestBody:
        content:
          application/json:
            schema: {$ref: '#/components/schemas/Upload'}
      responses:
        "200": {description: ok}
  /b/{id}:
    get:
      responses:
        "200": {description: ok}
components:
  schemas:
    Upload:
      type: string
      contentMediaType: application/octet-stream
    Widget:
      type: object
      ui_type: slider
`

func TestValidateAllReportsEveryFailure(t *testing.T) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData([]byte(twoErrorsSpec))
	require.NoError(t, err)

	ctx := openapi3.WithValidationOptions(context.Background(), openapi3.EnableJSONSchema2020Validation())
	failures := validateAll(ctx, doc, nil)

	var locations []string
	for _, f := range failures {
		locations = append(locations, f.Location)
	}
	require.Equal(t, []string{"components.schemas.Upload", "components.schemas.Widget", "paths./a.post", "paths./b/{id}.get"}, locations)

	var out bytes.Buffer
	printFailures(&out, failures)
	require.Contains(t, out.String(), "     2  extra sibling fields: [contentMediaType]\n")
	require.Contains(t, out.String(), "     1  extra sibling fields: [ui_type]\n")
	require.Contains(t, out.String(), "     1  operation GET /b/{id} must define exactly all path parameters (missing: [id])\n")

	ctx = openapi3.WithValidationOptions(ctx, openapi3.AllowExtraSiblingFields("contentMediaType", "ui_type"))
	failures = validateAll(ctx, doc, []string{"contentMediaType", "ui_type"})
	require.Len(t, failures, 1)
}

func TestValidateAllReportsDuplicateOperationIDs(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData([]byte(`
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /a: {get: {operationId: same, responses: {"200": {description: ok}}}}
  /b: {get: {operationId: same, responses: {"200": {description: ok}}}}
`))
	require.NoError(t, err)

	failures := validateAll(context.Background(), doc, nil)
	require.Len(t, failures, 1)
	require.EqualError(t, failures[0].Err, `operations "GET /a" and "GET /b" have the same operation id "same"`)
}
