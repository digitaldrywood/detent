package gate

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestCriterionEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		entries    []CriterionEvidence
		commands   []CommandResult
		reads      []AttachmentRead
		kind       string
		gaps       []string
		findings   int
		class      string
		receiptIDs []string
		mixed      bool
		edit       func(*ValidatorResult)
		tree       *string
	}{
		{name: "missing map", gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "unsatisfiable criterion", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceNotVerified, UnverifiedClass: " UNSATISFIABLE ", Reference: "Requires production"}}, class: CriterionUnsatisfiable, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "unknown class requires rework", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceNotVerified, UnverifiedClass: "unknown"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "failed proof cannot claim unsatisfiable", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, UnverifiedClass: CriterionUnsatisfiable, Reference: "make check"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "verified proof clears class", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceTest, UnverifiedClass: CriterionUnsatisfiable, Reference: "TestInvalid", Behavior: "Rejects invalid input"}}, gaps: []string{"Return valid input"}},
		{name: "named tests", entries: []CriterionEvidence{
			{Criterion: "Reject invalid input", Kind: EvidenceTest, Reference: "input_test.go:TestInvalid", Behavior: "Malformed input returns an error"},
			{Criterion: "Return valid input", Kind: EvidenceTest, Reference: "input_test.go:TestValid", Behavior: "Valid input returns its original value"},
		}},
		{name: "restated implementation", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceTest, Reference: "input_test.go:TestInvalid", Behavior: "Copies parser", RestatesImplementation: true}}, gaps: []string{"Reject invalid input", "Return valid input"}, findings: 1},
		{name: "rejected receipt test", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "go test", RestatesImplementation: true}}, gaps: []string{"Reject invalid input", "Return valid input"}, findings: 1},
		{name: "unexplained test", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceTest, Reference: "TestInvalid"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "duplicate criterion", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceTest, Reference: "TestOne", Behavior: "Error"}, {Criterion: "Reject invalid input", Kind: EvidenceTest, Reference: "TestTwo", Behavior: "Error"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "non-native criterion cannot classify unsatisfiable", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceNotVerified, UnverifiedClass: CriterionUnsatisfiable}}, edit: func(r *ValidatorResult) { r.VersionID = "" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "unknown criterion", entries: []CriterionEvidence{{Criterion: "Unrelated", Kind: EvidenceTest, Reference: "TestOne", Behavior: "Error"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "bound receipt", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "make check", HeadSHA: "head", TreeSHA: "tree", Behavior: "Malformed input returns an error"}}, commands: []CommandResult{{Command: "make check", HeadSHA: "head", TreeSHA: "tree"}}, gaps: []string{"Return valid input"}},
		{name: "missing receipt", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "make check", HeadSHA: "head", TreeSHA: "tree", Behavior: "Malformed input returns an error"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "failed receipt", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "make check", HeadSHA: "head", TreeSHA: "tree", Behavior: "Malformed input returns an error"}}, commands: []CommandResult{{Command: "make check", HeadSHA: "head", TreeSHA: "tree", ExitCode: 1}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "wrong head", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "make check", HeadSHA: "old", TreeSHA: "tree", Behavior: "Malformed input returns an error"}}, commands: []CommandResult{{Command: "make check", HeadSHA: "old", TreeSHA: "tree"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "wrong tree", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "make check", HeadSHA: "head", TreeSHA: "other", Behavior: "Malformed input returns an error"}}, commands: []CommandResult{{Command: "make check", HeadSHA: "head", TreeSHA: "other"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "multiple bound receipts", receiptIDs: []string{"timing_app", "timing_browser"}, gaps: []string{"Return valid input"}},
		{name: "copied receipt head typo", receiptIDs: []string{"timing_26cbfc84de51aae37078d2e3e5c4f5ed"}, tree: new("7c0cf6c077149f9d6057ca0b18260d117866efa6"), edit: func(r *ValidatorResult) {
			r.HeadSHA = "2c5b9460fc991683634196fd8e4fb9d62b99d39c"
			r.Commands = []CommandResult{{ReceiptID: "timing_26cbfc84de51aae37078d2e3e5c4f5ed", Command: "make check-land", ExitCode: 0, DurationNS: 188051296208, HeadSHA: r.HeadSHA, TreeSHA: "7c0cf6c077149f9d6057ca0b18260d117866efa6"}}
			r.CriteriaEvidence[0].Reference = "make check-land"
			r.CriteriaEvidence[0].HeadSHA = "2c5b9460fc991683196fd8e4fb9d62b99d39c"
			r.CriteriaEvidence[0].TreeSHA = "7c0cf6c077149f9d6057ca0b18260d117866efa6"
		}, gaps: []string{"Return valid input"}},
		{name: "omitted receipt source identity", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) {
			r.CriteriaEvidence[0].HeadSHA = ""
			r.CriteriaEvidence[0].TreeSHA = ""
		}, gaps: []string{"Return valid input"}},
		{name: "copied receipt tree typo", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].TreeSHA = "typo" }, gaps: []string{"Return valid input"}},
		{name: "missing reviewed head", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.HeadSHA = "" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "unknown reviewed head", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.HeadSHA = "unknown" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "host receipts supply missing finalization tree", receiptIDs: []string{"timing_app", "timing_browser"}, tree: new(""), edit: func(r *ValidatorResult) {
			r.CriteriaEvidence[0].HeadSHA = "typo"
			r.CriteriaEvidence[0].TreeSHA = "typo"
		}, gaps: []string{"Return valid input"}},
		{name: "missing trustworthy tree", receiptIDs: []string{"timing_app", "timing_browser"}, tree: new(""), edit: func(r *ValidatorResult) {
			for i := range r.Commands {
				r.Commands[i].TreeSHA = ""
			}
		}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "inconsistent trees without finalization", receiptIDs: []string{"timing_app", "timing_browser"}, tree: new(""), edit: func(r *ValidatorResult) { r.Commands[1].TreeSHA = "other" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "foreign head without finalization", receiptIDs: []string{"timing_app", "timing_browser"}, tree: new(""), edit: func(r *ValidatorResult) { r.Commands[1].HeadSHA = "other" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "unknown expected tree", receiptIDs: []string{"timing_app", "timing_browser"}, tree: new("unknown"), gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "missing selected receipt", receiptIDs: []string{"timing_app", "missing"}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "duplicate selected receipt", receiptIDs: []string{"timing_app", "timing_app"}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "empty selected receipt", receiptIDs: []string{"timing_app", ""}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "blank selected receipt", receiptIDs: []string{"timing_app", " "}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "ambiguous receipt ID", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.Commands = append(r.Commands, r.Commands[0]) }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "failed selected receipt", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.Commands[1].ExitCode = 1 }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "empty selected command", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.Commands[1].Command = "" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "selected receipt wrong head", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.Commands[1].HeadSHA = "other" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "selected receipt wrong tree", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.Commands[1].TreeSHA = "other" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "receipt IDs retain expected tree binding", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) {
			r.CriteriaEvidence[0].TreeSHA = "other"
			for i := range r.Commands {
				r.Commands[i].TreeSHA = "other"
			}
		}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "receipt IDs require behavior", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].Behavior = "" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "receipt IDs require reference", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].Reference = "" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "explicit not verified preserves gap", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].Kind = EvidenceNotVerified }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "receipt IDs reject restated implementation", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].RestatesImplementation = true }, gaps: []string{"Reject invalid input", "Return valid input"}, findings: 1},
		{name: "invalid ID cannot fall back to exact command", receiptIDs: []string{"missing"}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].Reference = r.Commands[0].Command }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "legacy combined command is not split", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].ReceiptIDs = nil }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "legacy receipt requires copied identity", receiptIDs: []string{"timing_app"}, edit: func(r *ValidatorResult) {
			r.CriteriaEvidence[0].ReceiptIDs = nil
			r.CriteriaEvidence[0].Reference = r.Commands[0].Command
			r.CriteriaEvidence[0].HeadSHA = ""
			r.CriteriaEvidence[0].TreeSHA = ""
		}, gaps: []string{"Reject invalid input", "Return valid input"}},

		{name: "1231 criterion 8 receipt list", kind: EvidenceAttachment, receiptIDs: []string{"timing_acd11650e031a5d126443f23f4e76017", "timing_c1dac16d7403a4347dc4d3c3c4a8e9b9", "attachment:att_d30e6c4e65ad29fa0ca0a5c05f158a62:9b4e9fb33661fc33301c4e7fe154e6fdcc12a9a07e70644880648010938458fc"},
			commands: []CommandResult{
				{ReceiptID: "timing_acd11650e031a5d126443f23f4e76017", Command: "[redacted]", HeadSHA: "head", TreeSHA: "tree"},
				{ReceiptID: "timing_c1dac16d7403a4347dc4d3c3c4a8e9b9", Command: "[redacted]", HeadSHA: "head", TreeSHA: "tree"},
			}, reads: []AttachmentRead{
				{ReceiptID: "attachment:att_d30e6c4e65ad29fa0ca0a5c05f158a62:9b4e9fb33661fc33301c4e7fe154e6fdcc12a9a07e70644880648010938458fc", AttachmentID: "att_d30e6c4e65ad29fa0ca0a5c05f158a62", SHA256: "9b4e9fb33661fc33301c4e7fe154e6fdcc12a9a07e70644880648010938458fc", ProjectID: "prj_example", HeadSHA: "head", TreeSHA: "tree"},
			}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].HeadSHA = ""; r.CriteriaEvidence[0].TreeSHA = "" }, gaps: []string{"Return valid input"}},
		{name: "1231 criterion 9 receipt list", kind: EvidenceAttachment, receiptIDs: []string{"timing_431cd79563fbf826a6fbdef39170d967", "timing_2446eb818dc03b11bc6aee4df26659f6", "timing_8a75dad48a4bd24e947b83a300ade1f6", "timing_62820c89f85a765c6a14b48da1967700", "timing_c76625d77fcee6aeda6d1c0ee4895f48", "timing_acd11650e031a5d126443f23f4e76017", "timing_c1dac16d7403a4347dc4d3c3c4a8e9b9", "attachment:att_51b76da935901799143716ab64790e67:c71544610a557c3ee6fd691c2b4b184db1732f36a259f87e2faddf12f7f3b832"},
			commands: []CommandResult{
				{ReceiptID: "timing_acd11650e031a5d126443f23f4e76017", Command: "[redacted]", HeadSHA: "head", TreeSHA: "tree"},
				{ReceiptID: "timing_431cd79563fbf826a6fbdef39170d967", Command: "git '--no-pager' '--literal-pathspecs' 'show' '--no-ext-diff' '--no-textconv' '087895fcd27646c859bdc9956fdea9fa75bb216c:docs/development.md'", HeadSHA: "head", TreeSHA: "tree"},
				{ReceiptID: "timing_c76625d77fcee6aeda6d1c0ee4895f48", Command: "make check-land", HeadSHA: "head", TreeSHA: "tree"},
				{ReceiptID: "timing_2446eb818dc03b11bc6aee4df26659f6", Command: "git '--no-pager' '--literal-pathspecs' 'show' '--no-ext-diff' '--no-textconv' '087895fcd27646c859bdc9956fdea9fa75bb216c:docs/invariants.md'", HeadSHA: "head", TreeSHA: "tree"},
				{ReceiptID: "timing_8a75dad48a4bd24e947b83a300ade1f6", Command: "git '--no-pager' '--literal-pathspecs' 'show' '--no-ext-diff' '--no-textconv' '087895fcd27646c859bdc9956fdea9fa75bb216c:docs/invariants.md'", HeadSHA: "head", TreeSHA: "tree"},
				{ReceiptID: "timing_c1dac16d7403a4347dc4d3c3c4a8e9b9", Command: "[redacted]", HeadSHA: "head", TreeSHA: "tree"},
				{ReceiptID: "timing_62820c89f85a765c6a14b48da1967700", Command: "git '--no-pager' '--literal-pathspecs' 'show' '--no-ext-diff' '--no-textconv' '087895fcd27646c859bdc9956fdea9fa75bb216c:docs/invariants.md'", HeadSHA: "head", TreeSHA: "tree"},
			}, reads: []AttachmentRead{
				{ReceiptID: "attachment:att_51b76da935901799143716ab64790e67:c71544610a557c3ee6fd691c2b4b184db1732f36a259f87e2faddf12f7f3b832", AttachmentID: "att_51b76da935901799143716ab64790e67", SHA256: "c71544610a557c3ee6fd691c2b4b184db1732f36a259f87e2faddf12f7f3b832", ProjectID: "prj_example", HeadSHA: "head", TreeSHA: "tree"},
			}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].HeadSHA = ""; r.CriteriaEvidence[0].TreeSHA = "" }, gaps: []string{"Return valid input"}},
		{name: "mixed visual and command evidence", mixed: true, gaps: []string{"Return valid input"}},
		{name: "mixed missing copied source", mixed: true, edit: func(r *ValidatorResult) {
			r.CriteriaEvidence[0].HeadSHA = ""
			r.CriteriaEvidence[0].TreeSHA = ""
		}, gaps: []string{"Return valid input"}},
		{name: "mixed copied source typo", mixed: true, edit: func(r *ValidatorResult) {
			r.CriteriaEvidence[0].HeadSHA = "typo"
			r.CriteriaEvidence[0].TreeSHA = "typo"
		}, gaps: []string{"Return valid input"}},
		{name: "mixed host receipts supply missing finalization tree", mixed: true, tree: new(""), gaps: []string{"Return valid input"}},
		{name: "mixed inconsistent trees without finalization", mixed: true, tree: new(""), edit: func(r *ValidatorResult) { r.AttachmentReads[1].TreeSHA = "other" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "mixed missing trustworthy tree", mixed: true, tree: new(""), edit: func(r *ValidatorResult) { r.AttachmentReads[0].TreeSHA = "" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "mixed ambiguous attachment receipt", mixed: true, edit: func(r *ValidatorResult) { r.AttachmentReads = append(r.AttachmentReads, r.AttachmentReads[0]) }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "mixed missing attachment digest", mixed: true, edit: func(r *ValidatorResult) { r.AttachmentReads[0].SHA256 = "" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "mixed command wrong head", mixed: true, edit: func(r *ValidatorResult) { r.Commands[0].HeadSHA = "other" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "mixed image wrong tree", mixed: true, edit: func(r *ValidatorResult) { r.AttachmentReads[1].TreeSHA = "other" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "mixed missing observed attachment", mixed: true, edit: func(r *ValidatorResult) { r.AttachmentReads = nil }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "mixed failed command", mixed: true, edit: func(r *ValidatorResult) { r.Commands[0].ExitCode = 1 }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "mixed command wrong tree", mixed: true, edit: func(r *ValidatorResult) { r.Commands[0].TreeSHA = "other" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "mixed image wrong head", mixed: true, edit: func(r *ValidatorResult) { r.AttachmentReads[1].HeadSHA = "other" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "mixed missing receipt", mixed: true, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].ReceiptIDs[2] = "missing" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "mixed duplicate selected receipt", mixed: true, edit: func(r *ValidatorResult) {
			r.CriteriaEvidence[0].ReceiptIDs = append(r.CriteriaEvidence[0].ReceiptIDs, "timing_app")
		}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "attachment requires image read", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceAttachment, Reference: "image.png", ReceiptIDs: []string{"timing_app"}, HeadSHA: "head", TreeSHA: "tree", Behavior: "The image shows an error"}}, commands: []CommandResult{{ReceiptID: "timing_app", Command: "make check-app", HeadSHA: "head", TreeSHA: "tree"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "observed attachment", edit: func(r *ValidatorResult) {
			r.CriteriaEvidence = []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceAttachment, Reference: "invalid-input.png", ReceiptIDs: []string{"image-read"}, HeadSHA: "head", TreeSHA: "tree", Behavior: "The capture shows the error beside the invalid field"}}
			r.AttachmentReads = []AttachmentRead{{ReceiptID: "image-read", AttachmentID: "attachment", SHA256: "digest", HeadSHA: "head", TreeSHA: "tree"}}
		}, gaps: []string{"Return valid input"}},
		{name: "invented attachment read", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceAttachment, Reference: "image.png", ReceiptIDs: []string{"invented"}, HeadSHA: "head", TreeSHA: "tree", Behavior: "Claimed image"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "attachment from another review", edit: func(r *ValidatorResult) {
			r.CriteriaEvidence = []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceAttachment, Reference: "image.png", ReceiptIDs: []string{"image-read"}, HeadSHA: "head", TreeSHA: "tree", Behavior: "Claimed image"}}
			r.AttachmentReads = []AttachmentRead{{ReceiptID: "image-read", AttachmentID: "attachment", SHA256: "digest", HeadSHA: "other", TreeSHA: "tree"}}
		}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "unsupported kind", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: "approval", Reference: "human"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
	}
	for _, test := range slices.Clone(tests) {
		if test.mixed || len(test.reads) > 0 {
			test.name += " as receipt"
			test.kind = EvidenceReceipt
			tests = append(tests, test)
		}
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := ValidatorResult{VersionID: "version", HeadSHA: "head", CriteriaEvidence: test.entries, Commands: test.commands, AttachmentReads: test.reads}
			if test.receiptIDs != nil {
				result.CriteriaEvidence = []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "make check-app; npx playwright test", ReceiptIDs: test.receiptIDs, HeadSHA: "head", TreeSHA: "tree", Behavior: "Both checks reject malformed input"}}
				if test.commands == nil {
					result.Commands = []CommandResult{{ReceiptID: "timing_app", Command: "make check-app", HeadSHA: "head", TreeSHA: "tree"}, {ReceiptID: "timing_browser", Command: "npx playwright test", HeadSHA: "head", TreeSHA: "tree"}}
				}
			}
			if test.mixed {
				result.CriteriaEvidence = []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceAttachment, Reference: "light.png, dark.png; make check-app", ReceiptIDs: []string{"light-read", "dark-read", "timing_app"}, HeadSHA: "head", TreeSHA: "tree", Behavior: "Light and dark narrow views show the error and the application checks pass"}}
				result.AttachmentReads = []AttachmentRead{{ReceiptID: "light-read", AttachmentID: "light", SHA256: "light-digest", HeadSHA: "head", TreeSHA: "tree"}, {ReceiptID: "dark-read", AttachmentID: "dark", SHA256: "dark-digest", HeadSHA: "head", TreeSHA: "tree"}}
				result.Commands = []CommandResult{{ReceiptID: "timing_app", Command: "make check-app", HeadSHA: "head", TreeSHA: "tree"}}
			}
			if test.kind != "" {
				result.CriteriaEvidence[0].Kind = test.kind
			}
			if test.edit != nil {
				test.edit(&result)
			}
			tree := "tree"
			if test.tree != nil {
				tree = *test.tree
			}
			var receiptIDs []string
			if len(result.CriteriaEvidence) > 0 {
				receiptIDs = slices.Clone(result.CriteriaEvidence[0].ReceiptIDs)
			}
			NormalizeCriterionEvidence(&result, []string{"Reject invalid input", "Return valid input"}, tree)
			if len(result.CriteriaEvidence) != 2 || result.CriteriaEvidence[0].Criterion != "Reject invalid input" || result.CriteriaEvidence[1].Criterion != "Return valid input" || !reflect.DeepEqual(result.NotVerified, test.gaps) || len(result.Findings) != test.findings {
				t.Fatalf("normalized evidence = %+v", result)
			}
			if len(result.NotVerified) == 1 && (test.mixed || len(test.reads) > 0) && (!slices.Equal(result.CriteriaEvidence[0].ReceiptIDs, receiptIDs) || result.CriteriaEvidence[0].Kind != EvidenceAttachment) {
				t.Fatalf("mixed evidence lost receipt identities or type: %+v", result.CriteriaEvidence[0])
			}
			wantClass := test.class
			if wantClass == "" && result.CriteriaEvidence[0].Kind == EvidenceNotVerified {
				wantClass = CriterionReworkFixable
			}
			if result.CriteriaEvidence[0].UnverifiedClass != wantClass {
				t.Fatalf("unverified class = %q, want %q", result.CriteriaEvidence[0].UnverifiedClass, wantClass)
			}
			wantTree := tree
			if wantTree == "" {
				wantTree = "tree"
			}
			if entry := result.CriteriaEvidence[0]; (entry.Kind == EvidenceReceipt || entry.Kind == EvidenceAttachment) && (entry.HeadSHA != result.HeadSHA || entry.TreeSHA != wantTree) {
				t.Fatalf("normalized receipt identity = %+v, want head %q and tree %q", entry, result.HeadSHA, wantTree)
			}
		})
	}
}

func TestCriterionEvidencePolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		policy      string
		verdict     string
		score       float64
		findings    []Finding
		gaps        []string
		entries     []CriterionEvidence
		wantVerdict string
		wantReason  Reason
	}{
		{name: "default reworks", verdict: "pass", score: .95, gaps: []string{"Reject invalid input"}, wantVerdict: "rework", wantReason: ReasonValidatorRework},
		{name: "unsatisfiable preserves blocking severity", verdict: "wait", score: .95, findings: []Finding{{Severity: "p1", Body: "Source defect"}}, gaps: []string{"Production client signs in"}, entries: []CriterionEvidence{{Criterion: "Production client signs in", Kind: EvidenceNotVerified, UnverifiedClass: CriterionUnsatisfiable}}, wantVerdict: "rework", wantReason: ReasonValidatorBlockedSeverity},
		{name: "unsatisfiable preserves minimum score", verdict: "wait", score: .2, gaps: []string{"Production client signs in"}, entries: []CriterionEvidence{{Criterion: "Production client signs in", Kind: EvidenceNotVerified, UnverifiedClass: CriterionUnsatisfiable}}, wantVerdict: "rework", wantReason: ReasonValidatorRework},
		{name: "unsatisfiable preserves explicit rework", verdict: "rework", score: .95, gaps: []string{"Production client signs in"}, entries: []CriterionEvidence{{Criterion: "Production client signs in", Kind: EvidenceNotVerified, UnverifiedClass: CriterionUnsatisfiable}}, wantVerdict: "rework", wantReason: ReasonValidatorRework},
		{name: "unsatisfiable waits under rework policy", verdict: "pass", score: .95, gaps: []string{"Production client signs in"}, entries: []CriterionEvidence{{Criterion: "Production client signs in", Kind: EvidenceNotVerified, UnverifiedClass: CriterionUnsatisfiable}}, wantVerdict: "wait", wantReason: ReasonValidatorWait},
		{name: "mixed criteria wait under rework policy", verdict: "pass", score: .95, gaps: []string{"Production client signs in", "Missing test"}, entries: []CriterionEvidence{{Criterion: "Production client signs in", Kind: EvidenceNotVerified, UnverifiedClass: CriterionUnsatisfiable}, {Criterion: "Missing test", Kind: EvidenceNotVerified, UnverifiedClass: CriterionReworkFixable}}, wantVerdict: "wait", wantReason: ReasonValidatorWait},
		{name: "unsatisfiable cannot disclose", policy: UnverifiedCriteriaDisclose, verdict: "pass", score: .95, gaps: []string{"Production client signs in"}, entries: []CriterionEvidence{{Criterion: "Production client signs in", Kind: EvidenceNotVerified, UnverifiedClass: CriterionUnsatisfiable}}, wantVerdict: "wait", wantReason: ReasonValidatorWait},
		{name: "disclosure passes", policy: UnverifiedCriteriaDisclose, verdict: "pass", score: .95, gaps: []string{"Reject invalid input"}, wantVerdict: "pass"},
		{name: "fully verified", verdict: "pass", score: .95, wantVerdict: "pass"},
		{name: "preserve validator error", verdict: "error", gaps: []string{"Reject invalid input"}, wantVerdict: "error", wantReason: ReasonValidatorError},
		{name: "disclosure preserves rework", policy: UnverifiedCriteriaDisclose, verdict: "rework", score: .95, gaps: []string{"Reject invalid input"}, wantVerdict: "rework", wantReason: ReasonValidatorRework},
		{name: "disclosure preserves wait", policy: UnverifiedCriteriaDisclose, verdict: "wait", score: .95, gaps: []string{"Reject invalid input"}, wantVerdict: "wait", wantReason: ReasonValidatorWait},
		{name: "disclosure preserves minimum score", policy: UnverifiedCriteriaDisclose, verdict: "pass", score: .5, gaps: []string{"Reject invalid input"}, wantVerdict: "pass", wantReason: ReasonValidatorScoreBelowThreshold},
		{name: "disclosure preserves severity", policy: UnverifiedCriteriaDisclose, verdict: "pass", score: .95, findings: []Finding{{Severity: "p1", Body: "Regression"}}, gaps: []string{"Reject invalid input"}, wantVerdict: "pass", wantReason: ReasonValidatorBlockedSeverity},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := ValidatorConfig{Enabled: true, MinScore: .8, BlockOn: []string{"p1"}, UnverifiedCriteria: test.policy}
			result := ValidatorResult{Submitted: true, Verdict: test.verdict, Score: test.score, Findings: test.findings, NotVerified: test.gaps, CriteriaEvidence: test.entries}
			ApplyCriterionPolicy(cfg, &result)
			decision, blocked := EvaluateValidator(cfg, result)
			if result.Verdict != test.wantVerdict || decision.Reason != test.wantReason || blocked != (test.wantReason != "") || !reflect.DeepEqual(result.NotVerified, test.gaps) {
				t.Fatalf("result=%+v decision=%+v blocked=%t", result, decision, blocked)
			}
			if test.name == "default reworks" && !strings.Contains(result.Summary, "Reject invalid input") {
				t.Fatalf("summary does not name the gap: %s", result.Summary)
			}
		})
	}
}
