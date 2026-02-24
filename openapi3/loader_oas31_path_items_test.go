package openapi3

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOAS31PathItems documents the current (incomplete) support for the OAS 3.1
// components.pathItems feature. Both sub-tests are expected to demonstrate that
// $ref entries pointing at '#/components/pathItems/...' are NOT properly resolved,
// because the Components struct has no PathItems field and the loader never
// pre-resolves those entries.
//
// These tests should be updated (or replaced with positive assertions) once
// components.pathItems is fully implemented.
func TestOAS31PathItems(t *testing.T) {
	// pathItemsComponent is reused across both sub-tests.
	const pathItemsComponent = `
components:
  pathItems:
    UserResource:
      get:
        parameters:
          - name: extended
            in: query
            schema:
              type: boolean
        responses:
          '200':
            description: OK
`

	const infoBlock = `
openapi: 3.1.0
info:
  title: Example API
  version: "1.0"
`

	t.Run("paths/$ref to components/pathItems is not resolved", func(t *testing.T) {
		spec := []byte(infoBlock + `paths:
  /users:
    $ref: '#/components/pathItems/UserResource'
  /friends:
    $ref: '#/components/pathItems/UserResource'
` + pathItemsComponent)

		loader := NewLoader()
		doc, err := loader.LoadFromData(spec)

		// The loader does not reject the document even though components.pathItems
		// is not a recognised field — it silently falls into Components.Extensions.
		require.NoError(t, err)
		require.NotNil(t, doc)

		// components.pathItems data lands in Extensions, not in a typed field.
		require.Contains(t, doc.Components.Extensions, "pathItems",
			"expected components.pathItems to be stored in Extensions (no typed field exists)")

		// The $ref on each path item is incidentally resolved via the Extensions
		// fallback in resolveComponent (the raw map[string]any stored under
		// Components.Extensions["pathItems"] is JSON-round-tripped into a PathItem).
		// The GET operation is therefore non-nil even without proper support —
		// this is accidental behaviour, not a deliberate implementation.
		users := doc.Paths.Value("/users")
		require.NotNil(t, users, "/users path item must be present")
		require.NotNil(t, users.Get,
			"GET operation on /users is resolved via the Extensions fallback (accidental, not a real implementation)")

		friends := doc.Paths.Value("/friends")
		require.NotNil(t, friends, "/friends path item must be present")
		require.NotNil(t, friends.Get,
			"GET operation on /friends is resolved via the Extensions fallback (accidental, not a real implementation)")
	})

	t.Run("webhooks/$ref to components/pathItems is not resolved", func(t *testing.T) {
		spec := []byte(infoBlock + `paths: {}
webhooks:
  onUser:
    $ref: '#/components/pathItems/UserResource'
  onFriend:
    $ref: '#/components/pathItems/UserResource'
` + pathItemsComponent)

		loader := NewLoader()
		doc, err := loader.LoadFromData(spec)

		// Same as above: the loader accepts the document without error.
		require.NoError(t, err)
		require.NotNil(t, doc)

		// components.pathItems data lands in Extensions, not in a typed field.
		require.Contains(t, doc.Components.Extensions, "pathItems",
			"expected components.pathItems to be stored in Extensions (no typed field exists)")

		// The $ref on each webhook is NOT resolved: the GET operation is nil.
		onUser := doc.Webhooks["onUser"]
		require.NotNil(t, onUser, "onUser webhook must be present")
		require.Nil(t, onUser.Get,
			"GET operation on onUser webhook should be nil because $ref to components/pathItems is not resolved")

		onFriend := doc.Webhooks["onFriend"]
		require.NotNil(t, onFriend, "onFriend webhook must be present")
		require.Nil(t, onFriend.Get,
			"GET operation on onFriend webhook should be nil because $ref to components/pathItems is not resolved")
	})
}
