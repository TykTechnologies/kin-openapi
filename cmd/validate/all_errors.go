package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// validationFailure is one element of a document that failed validation.
type validationFailure struct {
	Location string
	Err      error
}

// validateAll validates each element of doc separately and returns every failure,
// instead of stopping at the first one as doc.Validate does.
// Each element still reports only its own first error.
func validateAll(ctx context.Context, doc *openapi3.T, allowed []string) []validationFailure {
	var failures []validationFailure
	check := func(location string, err error) {
		if err != nil {
			failures = append(failures, validationFailure{Location: location, Err: err})
		}
	}

	if doc.OpenAPI == "" {
		check("openapi", errors.New("value of openapi must be a non-empty string"))
	}

	if doc.Info == nil {
		check("info", errors.New("must be an object"))
	} else {
		check("info", doc.Info.Validate(ctx))
	}
	if doc.Security != nil {
		check("security", doc.Security.Validate(ctx))
	}
	if doc.Servers != nil {
		check("servers", doc.Servers.Validate(ctx))
	}
	if doc.Tags != nil {
		check("tags", doc.Tags.Validate(ctx))
	}
	if doc.ExternalDocs != nil {
		check("externalDocs", doc.ExternalDocs.Validate(ctx))
	}
	check("extensions", unknownFields(doc.Extensions, allowed))

	if c := doc.Components; c != nil {
		validateComponents(ctx, c, check)
		check("components.extensions", unknownFields(c.Extensions, allowed))
	}

	if doc.Paths == nil {
		check("paths", errors.New("must be an object"))
	} else {
		validatePaths(ctx, doc.Paths, check)
	}

	for _, name := range sortedKeys(doc.Webhooks) {
		item := doc.Webhooks[name]
		if item == nil {
			check("webhooks."+name, errors.New("webhook is nil"))
			continue
		}
		check("webhooks."+name, item.Validate(ctx))
	}

	return failures
}

func validateComponents(ctx context.Context, c *openapi3.Components, check func(string, error)) {
	component := func(kind, name string, v interface {
		Validate(context.Context, ...openapi3.ValidationOption) error
	}) {
		location := "components." + kind + "." + name
		if err := openapi3.ValidateIdentifier(name); err != nil {
			check(location, err)
			return
		}
		check(location, v.Validate(ctx))
	}

	for _, k := range sortedKeys(c.Schemas) {
		component("schemas", k, c.Schemas[k])
	}
	for _, k := range sortedKeys(c.Parameters) {
		component("parameters", k, c.Parameters[k])
	}
	for _, k := range sortedKeys(c.RequestBodies) {
		component("requestBodies", k, c.RequestBodies[k])
	}
	for _, k := range sortedKeys(c.Responses) {
		component("responses", k, c.Responses[k])
	}
	for _, k := range sortedKeys(c.Headers) {
		component("headers", k, c.Headers[k])
	}
	for _, k := range sortedKeys(c.SecuritySchemes) {
		component("securitySchemes", k, c.SecuritySchemes[k])
	}
	for _, k := range sortedKeys(c.Examples) {
		component("examples", k, c.Examples[k])
	}
	for _, k := range sortedKeys(c.Links) {
		component("links", k, c.Links[k])
	}
	for _, k := range sortedKeys(c.Callbacks) {
		component("callbacks", k, c.Callbacks[k])
	}
}

// validatePaths validates every operation on its own, inside a one-path Paths so that
// the path parameter checks of Paths.Validate still apply.
func validatePaths(ctx context.Context, paths *openapi3.Paths, check func(string, error)) {
	pathMap := paths.Map()
	operationIDs := make(map[string]string)

	for _, path := range sortedKeys(pathMap) {
		item := pathMap[path]
		if item == nil {
			item = &openapi3.PathItem{}
		}

		operations := item.Operations()
		if len(operations) == 0 {
			check("paths."+path, openapi3.NewPaths(openapi3.WithPath(path, item)).Validate(ctx))
			continue
		}

		for _, method := range sortedKeys(operations) {
			op := operations[method]
			single := *item
			single.Connect, single.Delete, single.Get, single.Head, single.Options = nil, nil, nil, nil, nil
			single.Patch, single.Post, single.Put, single.Trace = nil, nil, nil, nil
			single.SetOperation(method, op)
			check("paths."+path+"."+strings.ToLower(method),
				openapi3.NewPaths(openapi3.WithPath(path, &single)).Validate(ctx))

			if op == nil || op.OperationID == "" {
				continue
			}
			endpoint := method + " " + path
			if dup, ok := operationIDs[op.OperationID]; ok {
				check("paths."+path+"."+strings.ToLower(method),
					fmt.Errorf("operations %q and %q have the same operation id %q", dup, endpoint, op.OperationID))
				continue
			}
			operationIDs[op.OperationID] = endpoint
		}
	}
}

// unknownFields mirrors the library's unexported validateExtensions check.
func unknownFields(extensions map[string]any, allowed []string) error {
	var unknowns []string
	for _, k := range sortedKeys(extensions) {
		if !strings.HasPrefix(k, "x-") && !slices.Contains(allowed, k) {
			unknowns = append(unknowns, k)
		}
	}
	if len(unknowns) != 0 {
		return fmt.Errorf("extra sibling fields: %+v", unknowns)
	}
	return nil
}

// rootCause strips the location prefixes that the validators add around an error,
// so that failures with the same cause can be grouped.
func rootCause(err error) string {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			break
		}
		err = next
	}
	msg := err.Error()
	for {
		trimmed := trimLocationPrefix(msg)
		if trimmed == msg {
			return msg
		}
		msg = trimmed
	}
}

// locationPrefixes are the wrapping prefixes that use %v instead of %w,
// so errors.Unwrap cannot remove them.
var locationPrefixes = []string{"invalid path ", "invalid operation ", "invalid components: ", "invalid paths: "}

func trimLocationPrefix(msg string) string {
	for _, p := range locationPrefixes {
		if !strings.HasPrefix(msg, p) {
			continue
		}
		rest := msg[len(p):]
		if p[len(p)-1] == ' ' {
			i := strings.Index(rest, ": ")
			if i < 0 {
				return msg
			}
			rest = rest[i+2:]
		}
		return rest
	}
	return msg
}

func printFailures(w io.Writer, failures []validationFailure) {
	for _, f := range failures {
		fmt.Fprintf(w, "%s: %v\n", f.Location, f.Err)
	}

	counts := make(map[string]int)
	for _, f := range failures {
		counts[rootCause(f.Err)]++
	}
	causes := sortedKeys(counts)
	sort.SliceStable(causes, func(i, j int) bool { return counts[causes[i]] > counts[causes[j]] })

	fmt.Fprintf(w, "\n%d failing elements, %d distinct causes:\n", len(failures), len(causes))
	for _, c := range causes {
		fmt.Fprintf(w, "%6d  %s\n", counts[c], c)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
