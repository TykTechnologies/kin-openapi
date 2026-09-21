package openapi3filter_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/stretchr/testify/require"
)

// GHSA-mmfr-pmjx-hw9w: a nil-pointer dereference in convertParseError (reached
// via ConvertErrors / ValidationErrorEncoder) lets an unauthenticated client
// crash a server with a single multipart/form-data request.
//
// convertParseError dereferences e.Parameter.In in the "query" branch without a
// nil guard. For request-body errors e.Parameter is always nil, and only the
// multipart decoder produces the nested *ParseError shape that reaches that
// branch, so a malformed scalar part (e.g. a non-numeric value for an integer
// property) panics.
//
// Sending the crafted request through ValidateRequest and then ConvertErrors
// must not panic; it must return a 400 with a meaningful message. The
// application/json control cases confirm the JSON body path is unaffected.
func TestGHSA_mmfr_pmjx_hw9w_ConvertErrors_NoPanic(t *testing.T) {
	spec := `
openapi: '3.0.3'
info:
  title: t
  version: '1.0.0'
paths:
  /upload:
    post:
      requestBody:
        required: true
        content:
          multipart/form-data:
            schema:
              type: object
              properties:
                age: {type: integer}
          application/json:
            schema:
              type: object
              properties:
                age: {type: integer}
      responses:
        '200': {description: ok}
`[1:]

	loader := openapi3.NewLoader()
	ctx := loader.Context

	doc, err := loader.LoadFromData([]byte(spec))
	require.NoError(t, err)
	require.NoError(t, doc.Validate(ctx))

	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	multipartBody := func() (string, string) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		require.NoError(t, w.WriteField("age", "notanumber"))
		require.NoError(t, w.Close())
		return w.FormDataContentType(), buf.String()
	}

	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
	}{
		{
			// The crash: multipart scalar part fails primitive parsing.
			// e.Parameter == nil and the cause is a nested *ParseError.
			name:       "multipart malformed scalar (the crash)",
			body:       "notanumber",
			wantStatus: http.StatusBadRequest,
		},
		{
			// Control: malformed JSON body. *ParseError whose cause is an
			// encoding/json error, so the type assertion fails and the safe
			// fallback branch is taken.
			name:        "json malformed body",
			contentType: "application/json",
			body:        `{"age": `,
			wantStatus:  http.StatusBadRequest,
		},
		{
			// Control: wrong-type JSON body. Routed through convertSchemaError
			// (422), never reaching convertParseError.
			name:        "json wrong-type body",
			contentType: "application/json",
			body:        `{"age": "notanumber"}`,
			wantStatus:  http.StatusUnprocessableEntity,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contentType, body := tc.contentType, tc.body
			if contentType == "" {
				contentType, body = multipartBody()
			}

			req, err := http.NewRequest(http.MethodPost, "/upload", strings.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", contentType)

			route, pathParams, err := router.FindRoute(req)
			require.NoError(t, err)

			reqErr := openapi3filter.ValidateRequest(ctx, &openapi3filter.RequestValidationInput{
				Request:    req,
				PathParams: pathParams,
				Route:      route,
				Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			})
			require.Error(t, reqErr)

			// This is what a typical error-rendering middleware calls; it must
			// not panic.
			var converted error
			require.NotPanics(t, func() {
				converted = openapi3filter.ConvertErrors(reqErr)
			})

			var validationErr *openapi3filter.ValidationError
			require.ErrorAs(t, converted, &validationErr)
			require.Equal(t, tc.wantStatus, validationErr.Status)
			require.NotEmpty(t, validationErr.Title, "converted error should carry a meaningful message")
		})
	}
}

// The fork reaches the vulnerable branch more readily than upstream v0.132 did.
// MultipartBodyDecoder runs parsePrimitive over any part sent with no
// Content-Type or with text/plain (req_resp_decoder.go, "Parse primitive types
// when no content type is explicitely provided"), and wraps a failure as
// &ParseError{path: []any{name}, Cause: v} -- Kind left at KindOther, Reason
// left empty. That is exactly the nested shape convertParseError's "query"
// branch inspects, and for a body error e.Parameter is nil.
//
// These cases walk every parsePrimitiveCase arm that yields KindInvalidFormat,
// plus both halves of the `contentType == "" || contentType == "text/plain"`
// condition, and pin the exact rendered Title. Asserting the full title matters:
// the outer ParseError carries no Reason, so without the innerErr.Error()
// fallback the fix would return a 400 with an empty body message.
func TestGHSA_mmfr_pmjx_hw9w_MultipartScalarKinds(t *testing.T) {
	spec := `
openapi: '3.0.3'
info:
  title: t
  version: '1.0.0'
paths:
  /upload:
    post:
      requestBody:
        required: true
        content:
          multipart/form-data:
            schema:
              type: object
              properties:
                age: {type: integer}
                small: {type: integer, format: int32}
                score: {type: number}
                active: {type: boolean}
                nested: {type: object, properties: {inner: {type: string}}}
      responses:
        '200': {description: ok}
`[1:]

	loader := openapi3.NewLoader()
	ctx := loader.Context

	doc, err := loader.LoadFromData([]byte(spec))
	require.NoError(t, err)
	require.NoError(t, doc.Validate(ctx))

	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		field  string
		value  string
		partCT string // Content-Type on the part itself, "" to omit

		// Either the converted ValidationError's status+title, or, when
		// convertParseError declines to convert, the untouched error's text.
		wantStatus      int
		wantTitle       string
		wantUnconverted string
	}{
		{
			name:       "integer",
			field:      "age",
			value:      "notanumber",
			wantStatus: http.StatusBadRequest,
			wantTitle:  "path age: value notanumber: an invalid integer: invalid syntax",
		},
		{
			// int32 takes the other branch of parsePrimitiveCase's "integer" arm.
			name:       "integer int32",
			field:      "small",
			value:      "99999999999999",
			wantStatus: http.StatusBadRequest,
			wantTitle:  "path small: value 99999999999999: an invalid integer: value out of range",
		},
		{
			name:       "number",
			field:      "score",
			value:      "notafloat",
			wantStatus: http.StatusBadRequest,
			wantTitle:  "path score: value notafloat: an invalid number: invalid syntax",
		},
		{
			name:       "boolean",
			field:      "active",
			value:      "notabool",
			wantStatus: http.StatusBadRequest,
			wantTitle:  "path active: value notabool: an invalid boolean: invalid syntax",
		},
		{
			// Same crash via the explicit text/plain half of the condition.
			name:       "integer with explicit text/plain part",
			field:      "age",
			value:      "notanumber",
			partCT:     "text/plain",
			wantStatus: http.StatusBadRequest,
			wantTitle:  "path age: value notanumber: an invalid integer: invalid syntax",
		},
		{
			// An object-typed part yields a KindOther inner error with no Cause of
			// its own, so innerErr.RootCause() is nil and convertParseError returns
			// nil before e.Parameter is ever read. ConvertErrors then hands back the
			// original *RequestError unconverted. Recorded here so it is visible
			// that this input never reaches the guarded deref -- and that the guard
			// is not what saves it.
			name:  "object-typed part is not converted",
			field: "nested",
			value: "notanobject",
			wantUnconverted: "request body has an error: failed to decode request body: " +
				"path nested: value notanobject: schema has non primitive type object",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := multipart.NewWriter(&buf)
			if tc.partCT == "" {
				require.NoError(t, w.WriteField(tc.field, tc.value))
			} else {
				h := make(textproto.MIMEHeader)
				h.Set("Content-Disposition", `form-data; name="`+tc.field+`"`)
				h.Set("Content-Type", tc.partCT)
				part, err := w.CreatePart(h)
				require.NoError(t, err)
				_, err = part.Write([]byte(tc.value))
				require.NoError(t, err)
			}
			require.NoError(t, w.Close())

			req, err := http.NewRequest(http.MethodPost, "/upload", bytes.NewReader(buf.Bytes()))
			require.NoError(t, err)
			req.Header.Set("Content-Type", w.FormDataContentType())

			route, pathParams, err := router.FindRoute(req)
			require.NoError(t, err)

			reqErr := openapi3filter.ValidateRequest(ctx, &openapi3filter.RequestValidationInput{
				Request:    req,
				PathParams: pathParams,
				Route:      route,
				Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			})
			require.Error(t, reqErr)

			var converted error
			require.NotPanics(t, func() {
				converted = openapi3filter.ConvertErrors(reqErr)
			})

			if tc.wantUnconverted != "" {
				require.Equal(t, tc.wantUnconverted, converted.Error())
				return
			}

			var validationErr *openapi3filter.ValidationError
			require.ErrorAs(t, converted, &validationErr)
			require.Equal(t, tc.wantStatus, validationErr.Status)
			require.Equal(t, tc.wantTitle, validationErr.Title)
		})
	}
}

// The nil guard must not change how query-parameter errors render: those reach
// the same branch with e.Parameter set, and the guard is an added conjunct, not
// a replacement. validation_error_test.go already covers this, but the
// assertion lives here too so the no-regression intent sits next to the fix.
func TestGHSA_mmfr_pmjx_hw9w_QueryParamTitleUnchanged(t *testing.T) {
	spec := `
openapi: '3.0.3'
info:
  title: t
  version: '1.0.0'
paths:
  /items:
    get:
      parameters:
        - name: ids
          in: query
          explode: false
          schema:
            type: array
            items: {type: integer}
      responses:
        '200': {description: ok}
`[1:]

	loader := openapi3.NewLoader()
	ctx := loader.Context

	doc, err := loader.LoadFromData([]byte(spec))
	require.NoError(t, err)
	require.NoError(t, doc.Validate(ctx))

	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, "/items?ids=1,notAnInt", nil)
	require.NoError(t, err)

	route, pathParams, err := router.FindRoute(req)
	require.NoError(t, err)

	reqErr := openapi3filter.ValidateRequest(ctx, &openapi3filter.RequestValidationInput{
		Request:    req,
		PathParams: pathParams,
		Route:      route,
		Options:    &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	})
	require.Error(t, reqErr)

	var validationErr *openapi3filter.ValidationError
	require.ErrorAs(t, openapi3filter.ConvertErrors(reqErr), &validationErr)
	require.Equal(t, http.StatusBadRequest, validationErr.Status)
	require.Equal(t,
		`parameter "ids" in query is invalid: notAnInt is an invalid integer`,
		validationErr.Title)
}
