package codex

import (
	"encoding/json"
	"strings"
)

// Codex 0.154's app-server accepts exactly four approval-policy variants on
// thread/start, thread/resume and turn/start:
//
//	untrusted | on-request | granular | never
//
// Detent's own configuration vocabulary is older and still spells the
// unattended policy `reject`, either as the bare word or as the mapping
// `{reject: {sandbox_approval: true, rules: true, mcp_elicitations: true}}`
// that config.Default() installs. Sending that word to Codex 0.154 kills the
// first turn of a project that never overrode it:
//
//	thread/start: Invalid request: unknown variant `reject`, expected one of
//	`untrusted`, `on-request`, `granular`, `never`
//
// `never` is the exact equivalent of what Detent means by `reject`: Codex
// never asks the caller to approve anything and refuses what it cannot run
// unattended. The translation happens on the wire only, so older config files
// stay valid and `codex.approval_policy` still reads back the value the
// operator wrote.
const (
	// codexApprovalPolicyNever is the Codex 0.154 variant Detent's `reject`
	// vocabulary maps onto.
	codexApprovalPolicyNever = "never"
)

// codexApprovalPolicies is the variant set Codex 0.154 accepts verbatim.
var codexApprovalPolicies = map[string]struct{}{
	"untrusted":  {},
	"on-request": {},
	"granular":   {},
	"never":      {},
}

// detentApprovalPolicyAliases maps Detent's own approval vocabulary onto the
// Codex variant with the same meaning.
var detentApprovalPolicyAliases = map[string]string{
	"reject": codexApprovalPolicyNever,
}

// wireApprovalPolicy translates a configured approval policy into the
// vocabulary the Codex app-server accepts. A value Detent does not recognise
// passes through untouched so a newer Codex variant needs no Detent release.
func wireApprovalPolicy(policy any) any {
	switch value := policy.(type) {
	case nil:
		return nil
	case string:
		return wireApprovalPolicyName(value)
	case map[string]any:
		return wireApprovalPolicyMap(value, policy)
	case json.RawMessage:
		return wireApprovalPolicyJSON(value, policy)
	case []byte:
		return wireApprovalPolicyJSON(value, policy)
	default:
		return policy
	}
}

// wireApprovalPolicyName translates one policy word. The comparison ignores
// case and treats `_` and `-` alike, because both spellings reach Detent
// through YAML.
func wireApprovalPolicyName(name string) string {
	normalized := normalizeApprovalPolicyName(name)
	if alias, ok := detentApprovalPolicyAliases[normalized]; ok {
		return alias
	}
	if _, ok := codexApprovalPolicies[normalized]; ok {
		return normalized
	}
	return name
}

// wireApprovalPolicyMap translates the single-key mapping form. Detent's
// `reject` mapping carries which request classes to refuse; Codex's `never`
// carries no payload, so the payload is dropped rather than forwarded under a
// key Codex would reject. Any other mapping passes through.
func wireApprovalPolicyMap(policy map[string]any, original any) any {
	if len(policy) != 1 {
		return original
	}
	for key := range policy {
		alias, ok := detentApprovalPolicyAliases[normalizeApprovalPolicyName(key)]
		if !ok {
			return original
		}
		return alias
	}
	return original
}

// wireApprovalPolicyJSON translates an already-encoded policy. The original
// bytes are returned unless the translation actually changes the policy.
func wireApprovalPolicyJSON(raw []byte, original any) any {
	if len(raw) == 0 {
		return original
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return original
	}
	translated := wireApprovalPolicy(decoded)
	name, ok := translated.(string)
	if !ok {
		return original
	}
	encoded, err := json.Marshal(name)
	if err != nil {
		return original
	}
	if string(encoded) == string(raw) {
		return original
	}
	return json.RawMessage(encoded)
}

func normalizeApprovalPolicyName(name string) string {
	normalized := strings.ToLower(strings.TrimSpace(name))
	return strings.ReplaceAll(normalized, "_", "-")
}
