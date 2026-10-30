#!/usr/bin/env bash
set -euo pipefail

worktree="${1:?usage: validate-windows-local.sh <checkout>}"
profile="${DETENT_WINDOWS_PROFILE:-detent-windows}"
context="colima-$profile"
container="${DETENT_WINDOWS_CONTAINER:-detent-windows}"
port="${DETENT_WINDOWS_SSH_PORT:-2222}"
state="${DETENT_PLATFORM_CACHE:-$HOME/.cache/detent-platforms}/windows"
toolchain="$(awk '$1 == "toolchain" {print $2}' "$worktree/go.mod")"
packages=(./internal/shell ./internal/procgroup ./internal/service ./internal/update ./internal/runnerauth)

mkdir -p "$state/oem"
if [ ! -f "$state/id_ed25519" ]; then
  ssh-keygen -q -t ed25519 -N '' -C detent-windows-validation -f "$state/id_ed25519"
fi
cp "$state/id_ed25519.pub" "$state/oem/authorized_keys"
cat > "$state/oem/install.bat" <<EOF
powershell -NoProfile -ExecutionPolicy Bypass -Command "Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0; Set-Service sshd -StartupType Automatic; Start-Service sshd; New-NetFirewallRule -Name sshd -DisplayName sshd -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22; New-ItemProperty -Path 'HKLM:\\SOFTWARE\\OpenSSH' -Name DefaultShell -Value 'C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe' -PropertyType String -Force; Copy-Item C:\\OEM\\authorized_keys C:\\ProgramData\\ssh\\administrators_authorized_keys; icacls C:\\ProgramData\\ssh\\administrators_authorized_keys /inheritance:r /grant 'Administrators:F' /grant 'SYSTEM:F'; Invoke-WebRequest https://go.dev/dl/$toolchain.windows-arm64.msi -OutFile C:\\go.msi; Start-Process msiexec -Wait -ArgumentList '/i C:\\go.msi /qn'"
EOF

if ! colima status -p "$profile" > /dev/null 2>&1; then
  colima start "$profile" --vm-type vz --nested-virtualization --cpu 6 --memory 12 --disk 120 --runtime docker
fi
if ! docker --context "$context" inspect "$container" > /dev/null 2>&1; then
  docker --context "$context" run -d --name "$container" --device /dev/kvm --device /dev/net/tun --cap-add NET_ADMIN --stop-timeout 120 \
    -e VERSION=11 -e RAM_SIZE=8G -e CPU_CORES=4 -e DISK_SIZE=64G -e USERNAME=detent -e PASSWORD=detent \
    -p "127.0.0.1:$port:22" -p 127.0.0.1:8006:8006 \
    -v "$state/oem:/oem" -v detent-windows-storage:/storage dockurr/windows-arm > /dev/null
elif [ "$(docker --context "$context" inspect -f '{{.State.Running}}' "$container")" != true ]; then
  docker --context "$context" start "$container" > /dev/null
fi

ssh_windows() {
  ssh -i "$state/id_ed25519" -p "$port" -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$state/known_hosts" -o ConnectTimeout=10 -o BatchMode=yes detent@127.0.0.1 "$@"
}
deadline=$((SECONDS + ${DETENT_WINDOWS_BOOT_TIMEOUT:-5400}))
until ssh_windows 'Test-Path "C:\Program Files\Go\bin\go.exe"' 2> /dev/null | tr -d '\r' | grep -qx True; do
  if [ "$SECONDS" -ge "$deadline" ]; then
    echo "Windows did not become ready; watch the install at http://127.0.0.1:8006" >&2
    exit 1
  fi
  sleep 20
done

ssh_windows 'if (Test-Path C:\src) { Remove-Item -Recurse -Force C:\src }; New-Item -ItemType Directory C:\src | Out-Null'
tar -C "$worktree" --exclude=./node_modules --exclude=./web/conversation/node_modules --exclude=./tmp -cf - . | ssh_windows 'tar -xf - -C C:\src'
ssh_windows "\$env:Path = 'C:\\Program Files\\Go\\bin;' + \$env:Path; \$env:GOTOOLCHAIN = 'local'; Set-Location C:\\src; go test -short -timeout=5m ${packages[*]}; if (\$LASTEXITCODE -ne 0) { exit \$LASTEXITCODE }; go test -timeout=5m -run '^TestPowerShellInstallScript' .; exit \$LASTEXITCODE"
