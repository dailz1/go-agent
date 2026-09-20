package agenttest

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dailz1/go-agent/tool"
)

func TestCloneSchemaCopiesExtendedSchemaGraph(t *testing.T) {
	var source tool.ParameterSchema
	if err := source.UnmarshalJSON([]byte(extendedRecordingSchema)); err != nil {
		t.Fatalf("unmarshal source: %v", err)
	}
	clone := cloneSchema(source)
	if !reflect.DeepEqual(clone, source) {
		t.Fatalf("clone changed schema: %#v", clone)
	}
	if reflect.ValueOf(clone.Properties).Pointer() == reflect.ValueOf(source.Properties).Pointer() {
		t.Fatal("properties map is aliased")
	}
	if &clone.Keywords[0].Value[0] == &source.Keywords[0].Value[0] {
		t.Fatal("keyword value is aliased")
	}
	if clone.AdditionalPropertiesSchema == source.AdditionalPropertiesSchema {
		t.Fatal("additional-properties schema is aliased")
	}
	clone.Keywords[0].Value[0] = 'x'
	if string(clone.Keywords[0].Value) == string(source.Keywords[0].Value) {
		t.Fatal("keyword mutation reached source")
	}
	got, err := json.Marshal(clone)
	if err == nil || len(got) != 0 {
		t.Fatalf("invalid mutated carrier marshaled as %s, %v", got, err)
	}
}

func TestCloneSchemaPreservesCyclesWithoutAliasing(t *testing.T) {
	properties := map[string]tool.Property{}
	properties["self"] = tool.Property{Properties: properties}
	item := new(tool.Property)
	item.Items = item
	source := tool.ParameterSchema{Properties: properties, AnyOf: []*tool.Property{item}}

	clone := cloneSchema(source)
	if reflect.ValueOf(clone.Properties).Pointer() == reflect.ValueOf(source.Properties).Pointer() {
		t.Fatal("cyclic properties map is aliased")
	}
	self := clone.Properties["self"]
	if reflect.ValueOf(self.Properties).Pointer() != reflect.ValueOf(clone.Properties).Pointer() {
		t.Fatal("map cycle topology was not preserved")
	}
	if clone.AnyOf[0] == item || clone.AnyOf[0].Items != clone.AnyOf[0] {
		t.Fatal("pointer cycle topology was not preserved")
	}
}

func TestCloneSchemaPreservesMutualMapCycle(t *testing.T) {
	first, second := map[string]tool.Property{}, map[string]tool.Property{}
	first["second"] = tool.Property{Properties: second}
	second["first"] = tool.Property{Properties: first}
	var codecState tool.Property
	if err := codecState.UnmarshalJSON([]byte(`{"$ref":""}`)); err != nil {
		t.Fatal(err)
	}
	first["codec"] = codecState
	source := tool.ParameterSchema{Properties: first}

	clone := cloneSchema(source)
	if !reflect.DeepEqual(clone, source) {
		t.Fatal("mutual-map clone changed structure or codec state")
	}
	if reflect.ValueOf(clone.Properties).Pointer() == reflect.ValueOf(first).Pointer() {
		t.Fatal("first map is aliased")
	}
	clonedSecond := clone.Properties["second"].Properties
	if reflect.ValueOf(clonedSecond).Pointer() == reflect.ValueOf(second).Pointer() {
		t.Fatal("second map is aliased")
	}
	if reflect.ValueOf(clonedSecond["first"].Properties).Pointer() != reflect.ValueOf(clone.Properties).Pointer() {
		t.Fatal("mutual map topology was not preserved")
	}
}

func TestCloneSchemaPreservesMixedMapPointerCycle(t *testing.T) {
	properties := map[string]tool.Property{}
	back := new(tool.Property)
	if err := back.UnmarshalJSON([]byte(`{"$ref":""}`)); err != nil {
		t.Fatal(err)
	}
	back.Properties = properties
	properties["items"] = tool.Property{Items: back}
	source := tool.ParameterSchema{Properties: properties}

	clone := cloneSchema(source)
	if !reflect.DeepEqual(clone, source) {
		t.Fatal("mixed-cycle clone changed structure or codec state")
	}
	clonedItem := clone.Properties["items"].Items
	if clonedItem == back {
		t.Fatal("mixed-cycle pointer is aliased")
	}
	if reflect.ValueOf(clonedItem.Properties).Pointer() != reflect.ValueOf(clone.Properties).Pointer() {
		t.Fatal("mixed map/pointer topology was not preserved")
	}
}

func TestCloneSchemaPreservesSharedDAGWithoutAliasing(t *testing.T) {
	var codecState tool.Property
	if err := codecState.UnmarshalJSON([]byte(`{"type":["string","null"],"description":""}`)); err != nil {
		t.Fatal(err)
	}
	shared := map[string]tool.Property{"codec": codecState}
	item := &tool.Property{Properties: shared}
	source := tool.ParameterSchema{Properties: map[string]tool.Property{
		"first":  {Properties: shared},
		"second": {Properties: shared},
		"items":  {Items: item},
	}}

	clone := cloneSchema(source)
	if !reflect.DeepEqual(clone, source) {
		t.Fatal("shared-DAG clone changed structure or codec state")
	}
	first := clone.Properties["first"].Properties
	second := clone.Properties["second"].Properties
	itemProperties := clone.Properties["items"].Items.Properties
	if reflect.ValueOf(first).Pointer() != reflect.ValueOf(second).Pointer() || reflect.ValueOf(first).Pointer() != reflect.ValueOf(itemProperties).Pointer() {
		t.Fatal("shared map topology was not preserved")
	}
	if reflect.ValueOf(first).Pointer() == reflect.ValueOf(shared).Pointer() {
		t.Fatal("shared map is aliased")
	}
	if clone.Properties["items"].Items == item {
		t.Fatal("shared items pointer is aliased")
	}
}
