package openapi3filter

import (
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// C-02: a deepObject query parameter lets a client pick the array index, and
// sliceMapToSlice sized the reconstructed slice from that index alone. A ~24
// byte query string (param[items][50000000]=x) therefore forced a multi-gigabyte
// allocation, and buildResObj immediately made a second slice of the same size.
//
// Schema maxItems cannot defend this: ValidateParameter only runs the schema
// after decodeStyledParameter has returned, by which point both slices exist.
// The guard has to live in the decoder, which is what maxSliceMapToSliceGap does.
//
// The tests below pin the two rejection paths (huge gap, negative index), the
// exact boundary either side of the cap, that the cap still holds through nested
// arrays, and that legitimate sparse arrays are untouched.

// decodeDeepObjectParam decodes query against a deepObject parameter named
// "param" with the given schema, exercising the real
// decodeStyledParameter -> DecodeObject -> makeObject -> buildResObj ->
// sliceMapToSlice path.
func decodeDeepObjectParam(t *testing.T, schema *openapi3.SchemaRef, query string) (any, error) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, "http://test.org/test?"+query, nil)
	require.NoError(t, err, "failed to create a test request")

	param := &openapi3.Parameter{
		Name: "param", In: "query", Style: "deepObject", Explode: explode, Schema: schema,
	}
	got, _, err := decodeStyledParameter(param, &RequestValidationInput{Request: req})
	return got, err
}

func TestC02_DeepObjectSparseArray_RejectsOversizedAndNegativeIndexes(t *testing.T) {
	stringArrayObject := objectOf("items", stringArraySchema)

	// Derived from the constant so the boundary cases track it if it is ever
	// retuned, rather than silently becoming interior cases.
	atCap := maxSliceMapToSliceGap       // gap == cap, allowed
	overCap := maxSliceMapToSliceGap + 1 // gap == cap+1, rejected

	for _, tc := range []struct {
		name   string
		schema *openapi3.SchemaRef
		query  string
		err    error
		want   any
	}{
		{
			// The attack: one supplied element at a huge index.
			name:   "huge index is rejected",
			schema: stringArrayObject,
			query:  "param[items][50000000]=x",
			err: &ParseError{
				path: []any{"items"}, Kind: KindInvalidFormat,
				Reason: "could not convert value map to array: array index 50000000 is too sparse relative to the 1 supplied items",
			},
		},
		{
			// Previously silently dropped: max stayed at -1, so the build loop
			// never ran and the caller got an empty array instead of an error.
			name:   "negative index is rejected",
			schema: stringArrayObject,
			query:  "param[items][-1]=x",
			err: &ParseError{
				path: []any{"items"}, Kind: KindInvalidFormat,
				Reason: "could not convert value map to array: array indexes must not be negative: -1",
			},
		},
		{
			// A negative index mixed with a valid one used to be dropped too,
			// quietly changing the array the application saw.
			name:   "negative index alongside a valid one is rejected",
			schema: stringArrayObject,
			query:  "param[items][-1]=a&param[items][0]=b",
			err: &ParseError{
				path: []any{"items"}, Kind: KindInvalidFormat,
				Reason: "could not convert value map to array: array indexes must not be negative: -1",
			},
		},
		{
			name:   "gap one over the cap is rejected",
			schema: stringArrayObject,
			query:  fmt.Sprintf("param[items][%d]=x", overCap),
			err: &ParseError{
				path: []any{"items"}, Kind: KindInvalidFormat,
				Reason: fmt.Sprintf("could not convert value map to array: array index %d is too sparse relative to the 1 supplied items", overCap),
			},
		},
		{
			// Unchanged behaviour, from req_resp_decoder_test.go's
			// "deepObject explode nested array of objects - missing intermediate
			// array index": a small gap is still filled with nils.
			name:   "small sparse array still decodes",
			schema: objectOf("arr", arrayOf(objectOf("key", booleanSchema))),
			query:  "param[arr][3][key]=true&param[arr][0][key]=false",
			want: map[string]any{"arr": []any{
				map[string]any{"key": false}, nil, nil, map[string]any{"key": true},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeDeepObjectParam(t, tc.schema, tc.query)

			if tc.err != nil {
				require.Error(t, err)
				matchParseError(t, err, tc.err)
				return
			}

			require.NoError(t, err)
			require.EqualValues(t, tc.want, got)
		})
	}

	t.Run("gap exactly at the cap still decodes", func(t *testing.T) {
		got, err := decodeDeepObjectParam(t, stringArrayObject, fmt.Sprintf("param[items][%d]=x", atCap))
		require.NoError(t, err)

		obj, ok := got.(map[string]any)
		require.True(t, ok, "want an object, got %T", got)
		items, ok := obj["items"].([]any)
		require.True(t, ok, "want items to be an array, got %T", obj["items"])
		require.Len(t, items, atCap+1)
		require.Equal(t, "x", items[atCap])
		require.Nil(t, items[0])
	})
}

// TestC02_DeepObjectSparseArray_NestingStaysBounded checks the cap composes:
// each level of an array-of-arrays is bounded independently, so nesting cannot
// multiply its way back to an unbounded allocation.
func TestC02_DeepObjectSparseArray_NestingStaysBounded(t *testing.T) {
	schema := objectOf("arr", arrayOf(stringArraySchema))

	t.Run("each level within the cap decodes", func(t *testing.T) {
		idx := maxSliceMapToSliceGap - 1
		got, err := decodeDeepObjectParam(t, schema, fmt.Sprintf("param[arr][%d][%d]=x", idx, idx))
		require.NoError(t, err)

		outer := got.(map[string]any)["arr"].([]any)
		require.Len(t, outer, idx+1)
		inner, ok := outer[idx].([]any)
		require.True(t, ok, "want a nested array, got %T", outer[idx])
		require.Len(t, inner, idx+1)
		require.Equal(t, "x", inner[idx])
	})

	t.Run("an over-cap inner index is rejected", func(t *testing.T) {
		_, err := decodeDeepObjectParam(t, schema,
			fmt.Sprintf("param[arr][0][%d]=x", maxSliceMapToSliceGap+1))
		require.Error(t, err)
		require.Contains(t, err.Error(), "too sparse")
	})
}

// TestC02_DeepObjectSparseArray_AllocationIsBounded is the test that actually
// pins the security property. The cases above would still pass if someone raised
// maxSliceMapToSliceGap to math.MaxInt and kept the error strings, because a
// large-enough index would still trip some check. This one fails outright if the
// decoder can be made to allocate proportionally to the index again.
//
// Unpatched, this request allocates roughly 50e6 * 16 bytes twice (once in
// sliceMapToSlice's append loop, once in buildResObj's make) -- around 1.6 GB.
func TestC02_DeepObjectSparseArray_AllocationIsBounded(t *testing.T) {
	const (
		index    = 50000000
		maxAlloc = 4 << 20 // 4 MiB of headroom over the few KiB actually needed
	)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	_, err := decodeDeepObjectParam(t, objectOf("items", stringArraySchema),
		fmt.Sprintf("param[items][%d]=x", index))

	runtime.ReadMemStats(&after)

	// Assert the allocation bound first and non-fatally: if the guard is gone
	// the decode succeeds, and this is the assertion that still reports why.
	allocated := after.TotalAlloc - before.TotalAlloc
	assert.Lessf(t, allocated, uint64(maxAlloc),
		"decoding param[items][%d]=x allocated %d bytes; the index must not drive the allocation",
		index, allocated)

	require.Error(t, err, "an index of %d must be rejected, not decoded", index)
	assert.Contains(t, err.Error(), "too sparse")
}

// TestC02_SliceMapToSlice covers the guard directly, including the inputs that
// are awkward to reach through a query string.
func TestC02_SliceMapToSlice(t *testing.T) {
	t.Run("dense input is unchanged", func(t *testing.T) {
		got, err := sliceMapToSlice(map[string]any{"0": "a", "1": "b"})
		require.NoError(t, err)
		require.Equal(t, []any{"a", "b"}, got)
	})

	t.Run("gap at the cap is allowed", func(t *testing.T) {
		got, err := sliceMapToSlice(map[string]any{strconv.Itoa(maxSliceMapToSliceGap): "x"})
		require.NoError(t, err)
		require.Len(t, got, maxSliceMapToSliceGap+1)
	})

	t.Run("gap over the cap is rejected", func(t *testing.T) {
		_, err := sliceMapToSlice(map[string]any{strconv.Itoa(maxSliceMapToSliceGap + 1): "x"})
		require.ErrorContains(t, err, "too sparse")
	})

	t.Run("supplying more elements raises the allowed max", func(t *testing.T) {
		// len(m) participates in the bound, so a genuinely large array that was
		// actually transmitted still decodes.
		m := make(map[string]any, maxSliceMapToSliceGap)
		for i := 0; i < maxSliceMapToSliceGap; i++ {
			m[strconv.Itoa(i)] = "x"
		}
		m[strconv.Itoa(2*maxSliceMapToSliceGap-1)] = "x"

		got, err := sliceMapToSlice(m)
		require.NoError(t, err)
		require.Len(t, got, 2*maxSliceMapToSliceGap)
	})

	t.Run("negative index is rejected", func(t *testing.T) {
		_, err := sliceMapToSlice(map[string]any{"-1": "x"})
		require.ErrorContains(t, err, "array indexes must not be negative: -1")
	})

	t.Run("non-integer index is still rejected", func(t *testing.T) {
		_, err := sliceMapToSlice(map[string]any{"nope": "x"})
		require.ErrorContains(t, err, "array indexes must be integers")
	})
}
