package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

func commandLimitReferences(contract commandContract) []string {
	refs := slices.Clone(contract.limitRefs)
	if refs == nil {
		refs = []string{}
	}
	target, projected := projectionTargetForCommand(contract.ID)
	if !projected && contract.ID != "drafts.preview" && contract.ID != "batch" {
		return refs
	}
	refs = append(refs, "output_projection.default_json_bytes", "output_projection.maximum_json_bytes")
	if !projected {
		return refs
	}
	refs = append(refs, "output_projection.fields_flag", "output_projection.max_bytes_flag")
	switch target {
	case projectionTargetMessage:
		refs = append(refs, "output_projection.message_fields", "output_projection.message_views", "output_projection.message_default_view", "output_projection.view_flag")
	case projectionTargetDraft:
		refs = append(refs, "output_projection.draft_fields", "output_projection.draft_views", "output_projection.view_flag")
		if contract.ID != "drafts.inspect" {
			refs = append(refs, "output_projection.draft_default_view")
		}
	case projectionTargetAttachment:
		refs = append(refs, "output_projection.attachment_fields", "output_projection.attachment_views", "output_projection.attachment_default_view", "output_projection.view_flag")
	case projectionTargetRaw:
		refs = append(refs, "output_projection.raw_fields", "output_projection.raw_views", "output_projection.raw_default_view", "output_projection.view_flag")
	case projectionTargetDraftList:
		refs = append(refs, "output_projection.draft_list_fields", "output_projection.draft_list_core_fields", "output_projection.draft_list_optional_fields")
	case projectionTargetListPage:
		refs = append(refs, "output_projection.list_page_fields")
	case projectionTargetSearchPage:
		refs = append(refs, "output_projection.search_page_fields")
	}
	if slices.Contains([]string{"messages.get", "messages.raw", "drafts.inspect"}, contract.ID) {
		refs = append(refs, "output_projection.export_flag", "output_projection.maximum_content_export_bytes")
	}
	return refs
}

// Keep the typed full contract, including false values. Selected serialization
// contains exactly the declared paths, never zero-value-based approximations.
func (limits capabilityLimits) MarshalJSON() ([]byte, error) {
	type fullLimits capabilityLimits
	full, err := marshalCLIJSON(fullLimits(limits))
	if err != nil {
		return nil, fmt.Errorf("encode full capability limits: %w", err)
	}
	if limits.selectedRefs == nil {
		return full, nil
	}
	var source map[string]json.RawMessage
	if err := json.Unmarshal(full, &source); err != nil {
		return nil, fmt.Errorf("decode full capability limits: %w", err)
	}
	selected := make(map[string]json.RawMessage)
	nested := make(map[string]map[string]json.RawMessage)
	for _, ref := range limits.selectedRefs {
		parent, child, hasChild := strings.Cut(ref, ".")
		value, exists := source[parent]
		if !exists {
			return nil, fmt.Errorf("unknown capability limit %q", ref)
		}
		if !hasChild {
			selected[parent] = value
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(value, &fields); err != nil {
			return nil, fmt.Errorf("decode capability limit %s: %w", parent, err)
		}
		value, exists = fields[child]
		if !exists {
			return nil, fmt.Errorf("unknown capability limit %q", ref)
		}
		if nested[parent] == nil {
			nested[parent] = make(map[string]json.RawMessage)
		}
		nested[parent][child] = value
	}
	for parent, fields := range nested {
		if _, complete := selected[parent]; complete {
			continue
		}
		value, err := marshalCLIJSON(fields)
		if err != nil {
			return nil, fmt.Errorf("encode capability limit %s: %w", parent, err)
		}
		selected[parent] = value
	}
	return marshalCLIJSON(selected)
}
