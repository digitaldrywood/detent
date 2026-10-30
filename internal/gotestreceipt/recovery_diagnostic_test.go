package gotestreceipt
import ("bytes"; "encoding/json"; "errors"; "fmt"; "go/parser"; "go/token"; "io"; "os/exec"; "path/filepath"; "sort"; "strings"; "testing")
func TestRecoverySourceEligibility(t *testing.T) {
 cmd := exec.CommandContext(t.Context(), "go", "list", "-e", "-deps", "-test", "-json", "./...")
 cmd.Dir = "../.."
 out, err := cmd.Output(); if err != nil { t.Fatal(err) }
 type entry struct { sourcePackage; Imports []string }
 pkgs := map[string]entry{}
 dec := json.NewDecoder(bytes.NewReader(out))
 for { var p entry; err := dec.Decode(&p); if errors.Is(err, io.EOF) { break }; if err != nil { t.Fatal(err) }; pkgs[p.ImportPath] = p }
 var names []string
 for name := range pkgs { if strings.HasSuffix(name, ".test") { names = append(names, name) } }; sort.Strings(names)
 eligible := 0
 for _, name := range names {
  seen := map[string]bool{}
  var visit func(string) error
  visit = func(name string) error {
   if seen[name] { return nil }; seen[name] = true
   p, ok := pkgs[name]; if !ok { return fmt.Errorf("dependency unavailable: %s",name) }
   if p.Error != nil { return fmt.Errorf("%s: %s",name,p.Error.Err) }
   if p.Standard { return nil }
   for _, dep := range p.Imports { if err := visit(dep); err != nil { return err } }
   if p.Name == "main" && strings.HasSuffix(name,".test") { return nil }
   if len(p.CgoFiles) != 0 || len(p.SFiles) != 0 { return fmt.Errorf("%s: native dependencies",name) }
   for _, file := range p.GoFiles {
    tree, err := parser.ParseFile(token.NewFileSet(),filepath.Join(p.Dir,file),nil,0); if err != nil { return err }
    if err := checkSource(tree); err != nil { return fmt.Errorf("%s/%s: %w",name,file,err) }
   }; return nil
  }
  if err := visit(name); err != nil { fmt.Printf("%s: REFUSED %v\n",name,err) } else { eligible++; fmt.Printf("%s: SOURCE ELIGIBLE (binary match and runtime input recording not checked)\n",name) }
 }
 fmt.Printf("SOURCE ELIGIBLE %d/%d; this is an upper bound on reusable binaries, not a reuse result\n",eligible,len(names))
}
