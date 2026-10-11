#!/usr/bin/env bash
set -euo pipefail

shard=${1:-1/1}
index=${shard%/*}
total=${shard#*/}
if ! [[ "$index" =~ ^[1-9][0-9]*$ && "$total" =~ ^[1-9][0-9]*$ ]] || [ "$index" -gt "$total" ]; then
    echo "usage: cover-packages.sh INDEX/TOTAL" >&2
    exit 2
fi

module=github.com/digitaldrywood/detent
# Seconds observed under coverage with -p 4; unlisted packages weigh 5.
# Shard 1 starts loaded because it also runs the workspace and web suites.
go list ./... | awk -v wanted="$index" -v total="$total" -v module="$module" '
BEGIN {
    weight[module "/internal/hubserver"] = 267
    weight[module "/internal/orchestrator"] = 209
    weight[module "/internal/hubclient"] = 132
    weight[module] = 127
    weight[module "/internal/runner"] = 82
    weight[module "/internal/connector/github"] = 79
    weight[module "/internal/cli"] = 71
    weight[module "/internal/cloudentry"] = 69
    weight[module "/internal/project"] = 67
    weight[module "/internal/config/configdoc"] = 54
    weight[module "/internal/store"] = 32
    weight[module "/tools/testgate"] = 26
    weight[module "/internal/invariants"] = 20
    load[1] = 240
}
$0 != module "/internal/workspace" && $0 != module "/internal/web" {
    packages[++count] = $0
    cost[count] = ($0 in weight) ? weight[$0] : 5
}
END {
    for (i = 1; i <= count; i++) {
        for (j = i + 1; j <= count; j++) {
            if (cost[j] > cost[i] || (cost[j] == cost[i] && packages[j] < packages[i])) {
                tmp = cost[i]; cost[i] = cost[j]; cost[j] = tmp
                tmp = packages[i]; packages[i] = packages[j]; packages[j] = tmp
            }
        }
    }
    for (i = 1; i <= count; i++) {
        target = 1
        for (s = 2; s <= total; s++) {
            if (load[s] < load[target]) target = s
        }
        load[target] += cost[i]
        if (target == wanted) print packages[i]
    }
}'
