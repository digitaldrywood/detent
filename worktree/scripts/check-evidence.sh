check_with_evidence() {
    local scope=$1
    shift
    local head tree os arch version started finished elapsed result command
    head=$(git rev-parse HEAD 2>/dev/null) || head=''
    tree=$(git rev-parse 'HEAD^{tree}' 2>/dev/null) || tree=''
    if [ -n "$(git status --porcelain 2>/dev/null)" ]; then tree=''; fi
    os=$(go env GOHOSTOS 2>/dev/null) || os=''
    arch=$(go env GOHOSTARCH 2>/dev/null) || arch=''
    version=$(go env GOVERSION 2>/dev/null) || version=''
    started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    elapsed=$SECONDS
    if "$@"; then result=0; else result=$?; fi
    if [ -n "$(git status --porcelain 2>/dev/null)" ] || [ "$(git rev-parse HEAD 2>/dev/null)" != "$head" ]; then tree=''; fi
    elapsed=$((SECONDS - elapsed))
    finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    command=$*
    command=${command//\\/\\\\}
    command=${command//\"/\\\"}
    printf 'detent-check-evidence: {"scope":"%s","command":"%s","head_sha":"%s","tree_sha":"%s","environment":{"os":"%s","architecture":"%s","go_version":"%s"},"exit_code":%s,"started_at":"%s","finished_at":"%s","duration_ns":%s,"duration_resolution_ns":1000000000}\n' "$scope" "$command" "$head" "$tree" "$os" "$arch" "$version" "$result" "$started" "$finished" "$((elapsed * 1000000000))"
    return "$result"
}
