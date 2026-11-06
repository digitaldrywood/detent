#!/usr/bin/env bash
set -euo pipefail

shard=${1:-1/1}
index=${shard%/*}
total=${shard#*/}
if ! [[ "$index" =~ ^[1-9][0-9]*$ && "$total" =~ ^[1-9][0-9]*$ ]] || [ "$index" -gt "$total" ]; then
    echo "usage: race-packages.sh INDEX/TOTAL" >&2
    exit 2
fi

module=github.com/digitaldrywood/detent
go list ./... | awk -v wanted="$index" -v total="$total" -v module="$module" '
BEGIN {
    weight[module "/internal/hubgithub"] = 1312.466
    weight[module "/internal/hubclient"] = 1132.227
    weight[module "/internal/cloudentry"] = 843.191
    weight[module "/internal/store"] = 33.383
    weight[module "/cmd/detent"] = 12.772
    weight[module "/internal/releaseaudit"] = 4.026
    weight[module "/internal/update"] = 2.873
    weight[module "/internal/config/global"] = 1.682
    weight[module "/internal/explain"] = 1.184
    weight[module "/internal/changerequest"] = 1.123
    weight[module "/internal/healthnotify"] = 1.118
    weight[module "/internal/routine"] = 1.088
    weight[module "/internal/lessons"] = 1.065
    weight[module "/internal/workspacesession"] = 1.058
    weight[module "/internal/procstart"] = 1.056
    weight[module "/internal/skills"] = 1.054
    weight[module "/internal/aidebug"] = 1.050
    weight[module "/internal/activehours"] = 1.047
    weight[module "/internal/coordination"] = 1.045
    weight[module "/internal/backendcapacity"] = 1.043
    weight[module "/internal/cloudorigin"] = 1.041
    weight[module "/internal/workpad"] = 1.038
    weight[module "/internal/store/storetest"] = 1.037
    weight[module "/internal/pause"] = 1.034
    weight[module "/internal/provenance"] = 1.030
    weight[module "/internal/displayorder"] = 1.028
    weight[module "/tools/invariantcheck"] = 1.023
    weight[module "/internal/scheduleowner"] = 1.022
    weight[module "/internal/operations"] = 1.018
    weight[module "/internal/web/ui/primitives"] = 1.016
    weight[module "/internal/serviceapi"] = 1.015
    weight[module "/internal/isolation"] = 1.014
    weight[module "/internal/mutation"] = 1.014
}
$0 != module "/internal/hubserver" {
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
