# Keep the two expensive suites on dedicated runners. Hash all other import
# paths so adding or reordering packages never moves existing packages.
$0 == "github.com/digitaldrywood/detent/internal/hubserver" { if (shard == 0) print; next }
$0 == "github.com/digitaldrywood/detent/internal/orchestrator" { if (shard == 1) print; next }
{
    alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-"
    hash = 0
    for (i = 1; i <= length($0); i++) {
        hash = (hash * 31 + index(alphabet, substr($0, i, 1))) % 65521
    }
    if (2 + hash % 2 == shard) print
}
