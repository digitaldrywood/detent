package invariants

import (
	"go/ast"
	"go/types"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

func checkAttemptCommands(t *testing.T, packagesByPath []*packages.Package) {
	t.Helper()
	const prefix = "github.com/digitaldrywood/detent/internal/"
	internal := map[string]*packages.Package{}
	for _, pkg := range packagesByPath {
		internal[pkg.PkgPath] = pkg
	}
	reached := map[string]bool{}
	var visit func(string)
	visit = func(path string) {
		if reached[path] || internal[path] == nil {
			return
		}
		reached[path] = true
		for dep := range internal[path].Imports {
			visit(dep)
		}
	}
	visit(prefix + "runner")
	visit(prefix + "workspace")
	backend := internal[prefix+"runner"].Types.Scope().Lookup("AgentBackend").Type().Underlying().(*types.Interface)
	for path, pkg := range internal {
		if pkg.Imports[prefix+"runner"] == nil || strings.Contains(path, "/internal/scenariotest") {
			continue
		}
		for _, name := range pkg.Types.Scope().Names() {
			object, ok := pkg.Types.Scope().Lookup(name).(*types.TypeName)
			if ok && types.Implements(types.NewPointer(object.Type()), backend) {
				visit(path)
				break
			}
		}
	}
	reached[prefix+"workspacerunner"] = true
	reached[prefix+"cli"] = true
	for path := range reached {
		pkg := internal[path]
		for _, file := range pkg.Syntax {
			filename := filepath.Base(pkg.Fset.Position(file.Pos()).Filename)
			for _, decl := range file.Decls {
				name := ""
				if function, ok := decl.(*ast.FuncDecl); ok {
					name = function.Name.Name
				}
				count := 0
				ast.Inspect(decl, func(node ast.Node) bool {
					id, ok := node.(*ast.Ident)
					if !ok {
						return true
					}
					fn, ok := pkg.TypesInfo.Uses[id].(*types.Func)
					if !ok || fn.Pkg() == nil {
						return true
					}
					raw := false
					switch fn.FullName() {
					case "(*os/exec.Cmd).Start", "(*os/exec.Cmd).Run", "(*os/exec.Cmd).Output", "(*os/exec.Cmd).CombinedOutput", "os.StartProcess", "syscall.Exec", "syscall.ForkExec", "syscall.StartProcess", "golang.org/x/sys/unix.Exec", "golang.org/x/sys/windows.CreateProcess", prefix + "procgroup.Start":
						raw = true
					}
					if fn.FullName() == prefix+"procgroup.StartAuthorized" && !authorizedTransport(strings.TrimPrefix(path, prefix), filename, name) {
						t.Errorf("%s: use the scoped command owner for a new native launch", pkg.Fset.Position(id.Pos()))
					}
					if raw {
						count++
						if count > rawAttemptCommandAllowance(strings.TrimPrefix(path, prefix), filename, name) {
							t.Errorf("%s: native command must use the acknowledged process launch", pkg.Fset.Position(id.Pos()))
						}
					}
					return true
				})
			}
		}
	}
}

func authorizedTransport(pkg, file, name string) bool {
	switch pkg + "/" + file + ":" + name {
	case "antigravity/process.go:RunTurn", "claudecode/process.go:RunTurn", "opencode/process.go:RunTurn", "piagent/process.go:RunTurn", "codex/transport.go:NewTransport", "workspace/hook.go:runHookCommand", "workspace/merge.go:runValidationCommand", "procgroup/identity.go:StartCommand":
		return true
	}
	return false
}

func rawAttemptCommandAllowance(pkg, file, function string) int {
	switch pkg + "/" + file + ":" + function {
	case "procgroup/identity.go:CommandOutput":
		return 2
	case "procgroup/identity.go:StartCommand", "procgroup/identity.go:StartAuthorized", "procgroup/start_other.go:Start",
		"procgroup/process_group_unix.go:startAcknowledged", "procgroup/process_group_windows.go:startWindows",
		"procgroup/inspection_other_unix.go:inspectProcessGroup", "procgroup/inspection_other_unix.go:inspectProcess", "procgroup/process_group_unix.go:observeProcesses",
		"releaseaudit/sandbox_cgroup_linux.go:runSandboxCgroupCommand",
		"gobudget/wrapper.go:RunWrapper", "gotestreceipt/wrapper.go:runTest", "gotestreceipt/wrapper.go:run",
		"cli/local_worker.go:runLocalWorkerLaunch", "cli/controller_process.go:startControllerProcess",
		"cli/cli.go:defaultCommandRunner", "cli/doctor.go:resolveDoctorCommandInDir", "cli/doctor.go:runDoctorCommandInDir", "cli/doctor.go:defaultGitWorkTree", "cli/doctor.go:defaultGitRemoteURL", "cli/doctor.go:defaultGitTracked", "cli/doctor_landing_credential.go:defaultDoctorLandingCredential", "cli/runner_checkout.go:cloneRunnerRepositoryWithCommandContext", "cli/runner_checkout_refresh.go:refreshRunnerCheckout", "cli/runner_diagnostics.go:probeRunnerProviderAuth", "cli/runner_service_user.go:registerRunnerAsUser", "cli/validate.go:newValidateCommand", "cli/ssh_runner.go:WorkerHostAvailable", "cli/ssh_runner.go:callSSH":
		return 1
	}
	return 0
}
