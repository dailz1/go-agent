package tool

func (p Property) booleanConflict() bool {
	if p.decoded {
		return !p.typeAbsent || !p.descriptionAbsent || len(p.Enum) != 0 || p.Nullable || len(p.Properties) != 0 || len(p.Required) != 0 || p.Items != nil || p.AdditionalProperties != nil || p.AdditionalPropertiesSchema != nil || len(p.AnyOf) != 0 || len(p.OneOf) != 0 || len(p.AllOf) != 0 || p.refPresent || len(p.Keywords) != 0
	}
	return p.Type != "" || p.Description != "" || len(p.Enum) != 0 || p.Nullable || len(p.Properties) != 0 || len(p.Required) != 0 || p.Items != nil || p.AdditionalProperties != nil || p.AdditionalPropertiesSchema != nil || len(p.AnyOf) != 0 || len(p.OneOf) != 0 || len(p.AllOf) != 0 || p.Ref != "" || len(p.Keywords) != 0
}

func (p Property) typeActive() bool {
	return !p.typeAbsent && (p.decoded || !p.hasExtendedFields() || p.Type != "")
}

func (p Property) descriptionActive() bool {
	return !p.descriptionAbsent && (p.decoded || !p.hasExtendedFields() || p.Description != "")
}

func (p Property) hasExtendedFields() bool {
	return p.Nullable || len(p.Properties) != 0 || p.Items != nil || p.AdditionalProperties != nil || p.AdditionalPropertiesSchema != nil || len(p.AnyOf) != 0 || len(p.OneOf) != 0 || len(p.AllOf) != 0 || p.Ref != "" || len(p.Keywords) != 0 || p.BooleanSchema != nil
}
