package detent_test

const fakeRulesetGHScript = `#!/usr/bin/env bash
case "$TEST_SCENARIO:$*" in
  "list-failure:"*"rulesets?includes_parents=true"*)
    printf '101\n'
    exit 42
    ;;
  "detail-failure:"*"rulesets?includes_parents=true"*|"success:"*"rulesets?includes_parents=true"*)
    printf '101\n202\n'
    ;;
  "empty:"*"rulesets?includes_parents=true"*)
    ;;
  "detail-failure:"*"rulesets/101"*|"success:"*"rulesets/101"*)
    printf '{"id":101}\n'
    ;;
  "detail-failure:"*"rulesets/202"*)
    printf '{"id":'
    exit 43
    ;;
  "success:"*"rulesets/202"*)
    printf '{"id":202}\n'
    ;;
  *)
    printf 'unexpected gh invocation: %s\n' "$*" >&2
    exit 44
    ;;
esac
`
