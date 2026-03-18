package searchattribute

import (
	"testing"

	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/server/common/searchattribute/sadefs"
)

func Test_IsValid(t *testing.T) {
	r := require.New(t)
	typeMap := NameTypeMap{customSearchAttributes: map[string]enumspb.IndexedValueType{
		"key1": enumspb.INDEXED_VALUE_TYPE_TEXT,
		"key2": enumspb.INDEXED_VALUE_TYPE_INT,
		"key3": enumspb.INDEXED_VALUE_TYPE_BOOL,
	}}

	isDefined := typeMap.IsDefined("RunId")
	r.True(isDefined)
	isDefined = typeMap.IsDefined("TemporalChangeVersion")
	r.True(isDefined)
	isDefined = typeMap.IsDefined("key1")
	r.True(isDefined)

	isDefined = NameTypeMap{}.IsDefined("key1")
	r.False(isDefined)
	isDefined = typeMap.IsDefined("key4")
	r.False(isDefined)
	isDefined = typeMap.IsDefined("NamespaceId")
	r.False(isDefined)
}

func Test_GetType(t *testing.T) {
	r := require.New(t)
	typeMap := NameTypeMap{customSearchAttributes: map[string]enumspb.IndexedValueType{
		"key1": enumspb.INDEXED_VALUE_TYPE_TEXT,
		"key2": enumspb.INDEXED_VALUE_TYPE_INT,
		"key3": enumspb.INDEXED_VALUE_TYPE_BOOL,
	}}

	ivt, err := typeMap.GetType("key1")
	r.NoError(err)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_TEXT, ivt)
	ivt, err = typeMap.GetType("key2")
	r.NoError(err)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_INT, ivt)
	ivt, err = typeMap.GetType("key3")
	r.NoError(err)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_BOOL, ivt)
	ivt, err = typeMap.GetType("RunId")
	r.NoError(err)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_KEYWORD, ivt)
	ivt, err = typeMap.GetType("TemporalChangeVersion")
	r.NoError(err)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_KEYWORD_LIST, ivt)
	ivt, err = typeMap.GetType("NamespaceId")
	r.Error(err)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_UNSPECIFIED, ivt)

	ivt, err = NameTypeMap{}.GetType("key1")
	r.Error(err)
	r.ErrorIs(err, sadefs.ErrInvalidName)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_UNSPECIFIED, ivt)
	ivt, err = typeMap.GetType("key4")
	r.Error(err)
	r.ErrorIs(err, sadefs.ErrInvalidName)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_UNSPECIFIED, ivt)
}

func Test_WithPredefinedSearchAttributes(t *testing.T) {
	r := require.New(t)

	customSA := map[string]enumspb.IndexedValueType{
		"CustomKey": enumspb.INDEXED_VALUE_TYPE_TEXT,
	}
	base := NewNameTypeMap(customSA)

	// Baseline: default predefined includes TemporalChangeVersion from sadefs.Predefined().
	r.True(base.IsDefined("TemporalChangeVersion"))
	ivt, err := base.GetType("TemporalChangeVersion")
	r.NoError(err)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_KEYWORD_LIST, ivt)

	// Custom attributes are preserved.
	ivt, err = base.GetType("CustomKey")
	r.NoError(err)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_TEXT, ivt)

	// Override predefined with a custom set that does NOT include TemporalChangeVersion.
	overriddenPredefined := map[string]enumspb.IndexedValueType{
		"MyPredefined": enumspb.INDEXED_VALUE_TYPE_DOUBLE,
	}
	overridden := base.WithPredefinedSearchAttributes(overriddenPredefined)

	// New predefined attribute is resolved.
	ivt, err = overridden.GetType("MyPredefined")
	r.NoError(err)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_DOUBLE, ivt)

	// TemporalChangeVersion is no longer found via predefined (only system SAs remain).
	r.False(overridden.IsDefined("TemporalChangeVersion"))

	// Custom attributes are still preserved after override.
	ivt, err = overridden.GetType("CustomKey")
	r.NoError(err)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_TEXT, ivt)

	// System search attributes (e.g. RunId) are still accessible.
	ivt, err = overridden.GetType("RunId")
	r.NoError(err)
	r.Equal(enumspb.INDEXED_VALUE_TYPE_KEYWORD, ivt)

	// Original base map is not mutated.
	r.True(base.IsDefined("TemporalChangeVersion"))
	r.False(base.IsDefined("MyPredefined"))
}
