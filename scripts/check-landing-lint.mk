.PHONY: landing-linter
landing-linter:
	@test "$(LANDING_LINTER)" = "$(GOLANGCI_LINT_RUN)"
ifneq ($(wildcard .custom-gcl.yml),)
	@grep -Fqx 'version: $(GOLANGCI_LINT_VERSION)' .custom-gcl.yml
	GOTOOLCHAIN="$(GOLANGCI_LINT_TOOLCHAIN)" bash scripts/build-custom-linter.sh "$(GOLANGCI_LINT_VERSION)" "$(HEFT_VERSION)" "$(LANDING_LINTER)" "$(TEST_PROCS)"
else
landing-linter: $(GOLANGCI_LINT_RUN)
endif
