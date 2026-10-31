package gotestreceipt

import (
 "context"
 "encoding/json"
 "os"
 "os/exec"
 "path/filepath"
 "testing"
)

func TestIssue1940ReceiptExperiment(t *testing.T) {
 root, err := filepath.Abs("../..")
 if err != nil { t.Fatal(err) }
 scratch := os.Getenv("TMPDIR")
 store := &memoryStore{}
 session, err := Start(t.Context(), store, nil, scratch, root)
 if err != nil { t.Fatal(err) }
 t.Cleanup(func() { if _, err := session.Close(context.WithoutCancel(t.Context())); err != nil { t.Error(err) } })
 executable, err := os.Executable()
 if err != nil { t.Fatal(err) }
 wrapper, err := Install(scratch, executable)
 if err != nil { t.Fatal(err) }
 env := os.Environ()
 for name, value := range session.Environment() { env = append(env, name+"="+value) }
 env = append(env, RootEnvironment+"="+root, "GOFLAGS="+Goflags("",wrapper),"GOMAXPROCS=4")
 run := func(label string) {
  t.Helper()
  t.Log(label)
  command := exec.CommandContext(t.Context(), "go", "test", "-short", "-timeout=120s", "./internal/hubserver", "./internal/hubhealth", "./internal/hubanalytics", "./internal/hubsql")
  command.Dir, command.Env = root, env
  output, err := command.CombinedOutput()
  t.Logf("%s\nerr=%v stats=%+v", output, err, session.Stats())
  if err != nil { t.Fatal(err) }
 }
 run("baseline")
 fixture := filepath.Join(root,"internal/hubhealth/health_rules_test.go")
 original, err := os.ReadFile(fixture)
 if err != nil { t.Fatal(err) }
 t.Cleanup(func() { if err := os.WriteFile(fixture,original,0600); err != nil { t.Error(err) } })
 changed := []byte(string(original))
 for i:=0; i+len("healthEvidenceLimit+5")<=len(changed); i++ { if string(changed[i:i+len("healthEvidenceLimit+5")])=="healthEvidenceLimit+5" { changed[i+len("healthEvidenceLimit+")]='6'; break } }
 if err := os.WriteFile(fixture,changed,0600); err != nil { t.Fatal(err) }
 run("health fixture +5 -> +6")
 store.mu.Lock()
 defer store.mu.Unlock()
 for _, receipt := range store.receipts { data, err := json.Marshal(receipt); if err != nil { t.Fatal(err) }; t.Log(string(data)) }
 if session.Stats().Hits < 3 { t.Errorf("unchanged packages did not reuse receipts: %+v",session.Stats()) }
}

func TestIssue1940TempRootReproduction(t *testing.T) {
 scratch:=os.Getenv("TMPDIR")
 root:=filepath.Join(scratch,"minimal-receipt-fixture")
 if err:=os.MkdirAll(root,0700);err!=nil{t.Fatal(err)}
 for name, data:=range map[string]string{"go.mod":"module example.test/fixture\n\ngo 1.26\n", "fixture_test.go":"package fixture\nimport \"testing\"\nfunc TestTemporaryDirectory(t *testing.T) { if t.TempDir()==\"\" { t.Fatal(\"missing directory\") } }\n"} {if err:=os.WriteFile(filepath.Join(root,name),[]byte(data),0600);err!=nil{t.Fatal(err)}}
 store:=&memoryStore{}
 session,err:=Start(t.Context(),store,nil,scratch,root);if err!=nil{t.Fatal(err)}
 t.Cleanup(func(){_,err:=session.Close(context.WithoutCancel(t.Context()));if err!=nil{t.Error(err)}})
 executable,err:=os.Executable();if err!=nil{t.Fatal(err)}
 wrapper,err:=Install(scratch,executable);if err!=nil{t.Fatal(err)}
 env:=os.Environ();for n,v:=range session.Environment(){env=append(env,n+"="+v)}
 env=append(env,RootEnvironment+"="+root,"GOFLAGS="+Goflags("",wrapper),"GOMAXPROCS=4")
 for _,label:=range []string{"first execution","unchanged execution"} {
  command:=exec.CommandContext(t.Context(),"go","test","-timeout=120s","./...");command.Dir=root;command.Env=env
  output,err:=command.CombinedOutput();t.Logf("%s output=%s err=%v stats=%+v",label,output,err,session.Stats());if err!=nil{t.Fatal(err)}
 }
 if session.Stats().Hits==0{t.Fatal("minimal t.TempDir test does not record or reuse")}
}
