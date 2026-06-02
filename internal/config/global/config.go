package global

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/selector"
)

const (
	APIVersion = "detent/v1"
	Kind       = "GlobalConfig"

	SchedulingWeighted   = "weighted"
	SchedulingStrict     = "strict"
	SchedulingRoundRobin = "round_robin"
	SchedulingFairShare  = "fair_share"
)

var schedulingModes = []string{
	SchedulingWeighted,
	SchedulingStrict,
	SchedulingRoundRobin,
	SchedulingFairShare,
}

type PathRule string

const (
	PathRuleFlag                PathRule = "--config"
	PathRuleEnvConfig           PathRule = "CONFIG"
	PathRuleDeprecatedEnvConfig PathRule = "DETENT_CONFIG"
	PathRuleEnvHome             PathRule = "CONFIG_HOME"
	PathRuleDeprecatedEnvHome   PathRule = "DETENT_HOME"
	PathRuleUserConfigDir       PathRule = "os.UserConfigDir()"
	PathRuleLegacyHome          PathRule = "~/.detent"
)

type PathResolution struct {
	Path string
	Rule PathRule
}

type Config struct {
	Path       string    `yaml:"-"`
	APIVersion string    `yaml:"apiVersion"`
	Kind       string    `yaml:"kind"`
	Global     Settings  `yaml:"global"`
	Projects   []Project `yaml:"projects"`
}

type Settings struct {
	MaxConcurrentAgents int            `yaml:"max_concurrent_agents"`
	Scheduling          string         `yaml:"scheduling"`
	Identity            Identity       `yaml:"identity,omitempty"`
	FairShare           map[string]any `yaml:"fair_share,omitempty"`
	Startup             map[string]any `yaml:"startup,omitempty"`
}

type Project struct {
	ID            string            `yaml:"id"`
	Workflow      string            `yaml:"workflow"`
	Workdir       string            `yaml:"workdir"`
	Weight        int               `yaml:"weight"`
	Priority      int               `yaml:"priority"`
	Paused        bool              `yaml:"paused,omitempty"`
	CredentialRef string            `yaml:"credential_ref,omitempty"`
	Authorization selector.Selector `yaml:"authorization,omitempty"`
	Identity      Identity          `yaml:"-"`
}

type Identity = workflowconfig.Identity

type Option func(*options)

type MissingFileError struct {
	Path string
	Err  error
}

type ParseError struct {
	Path string
	Err  error
}

type ValidationError struct {
	Path     string
	Problems []string
}

type options struct {
	home                string
	relativeTo          string
	projectPathLiterals bool
}

type pathOptions struct {
	config        options
	lookupEnv     func(string) string
	userConfigDir func() (string, error)
	stat          func(string) (os.FileInfo, error)
}

func WithHome(home string) Option {
	return func(opts *options) {
		opts.home = home
	}
}

func WithRelativeTo(path string) Option {
	return func(opts *options) {
		opts.relativeTo = path
	}
}

func WithProjectPathLiterals() Option {
	return func(opts *options) {
		opts.projectPathLiterals = true
	}
}

func ResolvePath(configPath string) (PathResolution, error) {
	return resolvePath(configPath, defaultPathOptions())
}

func DefaultPath() (string, error) {
	resolution, err := ResolvePath("")
	if err != nil {
		return "", err
	}
	return resolution.Path, nil
}

func Default() (Config, error) {
	path, err := DefaultPath()
	if err != nil {
		return Config{}, err
	}
	return defaultConfig(path), nil
}

func DefaultAt(path string, opts ...Option) (Config, error) {
	if strings.TrimSpace(path) == "" {
		return Config{}, errors.New("global config path is required")
	}

	readOptions := defaultOptions()
	for _, opt := range opts {
		opt(&readOptions)
	}

	expandedPath, err := expandPath(path, readOptions)
	if err != nil {
		return Config{}, err
	}
	return defaultConfig(expandedPath), nil
}

func Read(path string, opts ...Option) (Config, error) {
	readOptions := defaultOptions()
	for _, opt := range opts {
		opt(&readOptions)
	}

	expandedPath, err := expandPath(path, readOptions)
	if err != nil {
		return Config{}, err
	}

	raw, err := os.ReadFile(expandedPath)
	if err != nil {
		return Config{}, MissingFileError{Path: expandedPath, Err: err}
	}

	return Parse(raw, expandedPath, opts...)
}

func Write(path string, cfg Config, opts ...Option) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("global config path is required")
	}

	writeOptions := defaultOptions()
	for _, opt := range opts {
		opt(&writeOptions)
	}

	expandedPath, err := expandPath(path, writeOptions)
	if err != nil {
		return err
	}

	cfg.Path = expandedPath
	if err := cfg.Validate(opts...); err != nil {
		return err
	}

	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal global config %s: %w", expandedPath, err)
	}

	if err := os.MkdirAll(filepath.Dir(expandedPath), 0o755); err != nil {
		return fmt.Errorf("create global config directory %s: %w", filepath.Dir(expandedPath), err)
	}
	if err := os.WriteFile(expandedPath, raw, 0o644); err != nil {
		return fmt.Errorf("write global config %s: %w", expandedPath, err)
	}

	return nil
}

func ReadOrDefault(path string, opts ...Option) (Config, error) {
	readOptions := defaultOptions()
	for _, opt := range opts {
		opt(&readOptions)
	}

	expandedPath, err := expandPath(path, readOptions)
	if err != nil {
		return Config{}, err
	}

	cfg, err := Read(expandedPath, opts...)
	if err == nil {
		return cfg, nil
	}

	var missing MissingFileError
	if errors.As(err, &missing) && errors.Is(missing.Err, os.ErrNotExist) {
		return defaultConfig(expandedPath), nil
	}

	return Config{}, err
}

func Parse(raw []byte, path string, opts ...Option) (Config, error) {
	readOptions := defaultOptions()
	for _, opt := range opts {
		opt(&readOptions)
	}

	var decoded any
	if err := yaml.Unmarshal(raw, &decoded); err != nil {
		return Config{}, ParseError{Path: path, Err: err}
	}

	root, ok := normalizeYAML(decoded).(map[string]any)
	if !ok {
		return Config{}, ValidationError{Path: path, Problems: []string{"root: must be a mapping"}}
	}

	if problems := validateRaw(root, readOptions); len(problems) > 0 {
		return Config{}, ValidationError{Path: path, Problems: problems}
	}

	cfg, err := build(root, path, readOptions)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(opts...); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c Config) Validate(opts ...Option) error {
	readOptions := defaultOptions()
	for _, opt := range opts {
		opt(&readOptions)
	}

	var problems []string
	if strings.TrimSpace(c.APIVersion) == "" {
		problems = append(problems, "apiVersion: is required")
	} else if c.APIVersion != APIVersion {
		problems = append(problems, "apiVersion: must equal "+APIVersion)
	}
	if strings.TrimSpace(c.Kind) == "" {
		problems = append(problems, "kind: is required")
	} else if c.Kind != Kind {
		problems = append(problems, "kind: must equal "+Kind)
	}

	if c.Global.MaxConcurrentAgents <= 0 {
		problems = append(problems, "global.max_concurrent_agents: must be a positive integer")
	}
	if !validSchedulingMode(c.Global.Scheduling) {
		problems = append(problems, "global.scheduling: must be one of "+strings.Join(schedulingModes, ", "))
	}
	problems = append(problems, c.Global.Identity.Validate("global.identity")...)
	problems = append(problems, startupErrors(c.Global.Startup, "global.startup")...)

	if c.Projects == nil {
		problems = append(problems, "projects: is required")
	}
	for index, project := range c.Projects {
		prefix := fmt.Sprintf("projects[%d]", index)
		if strings.TrimSpace(project.ID) == "" {
			problems = append(problems, prefix+".id: must not be blank")
		}
		problems = append(problems, projectPathErrors(project.Workflow, prefix+".workflow", readOptions, wantFile)...)
		problems = append(problems, projectPathErrors(project.Workdir, prefix+".workdir", readOptions, wantDirectory)...)
		if project.Weight <= 0 {
			problems = append(problems, prefix+".weight: must be a positive integer")
		}
		if project.CredentialRef != "" && strings.TrimSpace(project.CredentialRef) == "" {
			problems = append(problems, prefix+".credential_ref: must not be blank")
		}
		problems = append(problems, project.Authorization.Validate(prefix+".authorization")...)
	}
	problems = append(problems, duplicateProjectIDErrorsFromProjects(c.Projects)...)

	if len(problems) > 0 {
		return ValidationError{Path: c.Path, Problems: problems}
	}

	return nil
}

func (e MissingFileError) Error() string {
	return fmt.Sprintf("read global config %s: %v", e.Path, e.Err)
}

func (e MissingFileError) Unwrap() error {
	return e.Err
}

func (e ParseError) Error() string {
	return fmt.Sprintf("parse global config %s: %v", e.Path, e.Err)
}

func (e ParseError) Unwrap() error {
	return e.Err
}

func (e ValidationError) Error() string {
	if e.Path == "" {
		return "invalid global config: " + strings.Join(e.Problems, "; ")
	}
	return "invalid global config at " + e.Path + ": " + strings.Join(e.Problems, "; ")
}

func defaultOptions() options {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}

	return options{
		home:       home,
		relativeTo: cwd,
	}
}

func defaultPathOptions() pathOptions {
	return pathOptions{
		config:        defaultOptions(),
		lookupEnv:     os.Getenv,
		userConfigDir: os.UserConfigDir,
		stat:          os.Stat,
	}
}

func resolvePath(configPath string, opts pathOptions) (PathResolution, error) {
	opts = normalizePathOptions(opts)

	if strings.TrimSpace(configPath) != "" {
		return pathResolution(configPath, PathRuleFlag, opts.config)
	}
	if envPath, rule := lookupPathEnv(opts.lookupEnv, pathEnvCandidates{
		{name: string(PathRuleEnvConfig), rule: PathRuleEnvConfig},
		{name: string(PathRuleDeprecatedEnvConfig), rule: PathRuleDeprecatedEnvConfig},
	}); envPath != "" {
		return pathResolution(envPath, rule, opts.config)
	}
	if configHome, rule := lookupPathEnv(opts.lookupEnv, pathEnvCandidates{
		{name: string(PathRuleEnvHome), rule: PathRuleEnvHome},
		{name: string(PathRuleDeprecatedEnvHome), rule: PathRuleDeprecatedEnvHome},
	}); configHome != "" {
		expanded, err := expandPath(configHome, opts.config)
		if err != nil {
			return PathResolution{}, err
		}
		return PathResolution{Path: filepath.Join(expanded, "global.yaml"), Rule: rule}, nil
	}

	nativePath, nativeErr := userConfigPath(opts)
	legacyPath, legacyErr := legacyConfigPath(opts.config)
	switch {
	case nativeErr == nil && existingConfigFile(nativePath, opts):
		return PathResolution{Path: nativePath, Rule: PathRuleUserConfigDir}, nil
	case legacyErr == nil && existingConfigFile(legacyPath, opts):
		return PathResolution{Path: legacyPath, Rule: PathRuleLegacyHome}, nil
	case nativeErr == nil:
		return PathResolution{Path: nativePath, Rule: PathRuleUserConfigDir}, nil
	case legacyErr == nil:
		return PathResolution{Path: legacyPath, Rule: PathRuleLegacyHome}, nil
	default:
		return PathResolution{}, nativeErr
	}
}

type pathEnvCandidate struct {
	name string
	rule PathRule
}

type pathEnvCandidates []pathEnvCandidate

func lookupPathEnv(lookupEnv func(string) string, candidates pathEnvCandidates) (string, PathRule) {
	for _, candidate := range candidates {
		if value := strings.TrimSpace(lookupEnv(candidate.name)); value != "" {
			return value, candidate.rule
		}
	}
	return "", ""
}

func normalizePathOptions(opts pathOptions) pathOptions {
	if opts.config.home == "" && opts.config.relativeTo == "" {
		opts.config = defaultOptions()
	}
	if opts.lookupEnv == nil {
		opts.lookupEnv = os.Getenv
	}
	if opts.userConfigDir == nil {
		opts.userConfigDir = os.UserConfigDir
	}
	if opts.stat == nil {
		opts.stat = os.Stat
	}
	return opts
}

func pathResolution(path string, rule PathRule, opts options) (PathResolution, error) {
	expanded, err := expandPath(strings.TrimSpace(path), opts)
	if err != nil {
		return PathResolution{}, err
	}
	return PathResolution{Path: expanded, Rule: rule}, nil
}

func userConfigPath(opts pathOptions) (string, error) {
	dir, err := opts.userConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "detent", "global.yaml"), nil
}

func legacyConfigPath(opts options) (string, error) {
	if opts.home == "" {
		return "", errors.New("home directory is not available")
	}
	return filepath.Join(opts.home, ".detent", "global.yaml"), nil
}

func existingConfigFile(path string, opts pathOptions) bool {
	info, err := opts.stat(path)
	return err == nil && info.Mode().IsRegular()
}

func defaultConfig(path string) Config {
	return Config{
		Path:       path,
		APIVersion: APIVersion,
		Kind:       Kind,
		Global:     defaultSettings(),
		Projects:   []Project{},
	}
}

func defaultSettings() Settings {
	return Settings{
		MaxConcurrentAgents: 8,
		Scheduling:          SchedulingWeighted,
		FairShare: map[string]any{
			"half_life": "1h",
		},
		Startup: map[string]any{
			"jitter_seconds":       10,
			"max_spawn_per_second": 2,
		},
	}
}

func validateRaw(attrs map[string]any, opts options) []string {
	var problems []string

	problems = append(problems, requiredErrors(attrs, []string{"apiVersion", "kind", "global", "projects"})...)
	problems = append(problems, versionErrors(attrs["apiVersion"])...)
	problems = append(problems, kindErrors(attrs["kind"])...)
	problems = append(problems, globalErrors(attrs["global"])...)
	problems = append(problems, projectsErrors(attrs["projects"], opts)...)

	return problems
}

func requiredErrors(attrs map[string]any, fields []string) []string {
	var problems []string
	for _, field := range fields {
		value, ok := attrs[field]
		if !ok || value == nil {
			problems = append(problems, field+": is required")
		}
	}
	return problems
}

func versionErrors(value any) []string {
	if value == nil {
		return nil
	}
	text, ok := value.(string)
	if !ok || text != APIVersion {
		return []string{"apiVersion: must equal " + APIVersion}
	}
	return nil
}

func kindErrors(value any) []string {
	if value == nil {
		return nil
	}
	text, ok := value.(string)
	if !ok || text != Kind {
		return []string{"kind: must equal " + Kind}
	}
	return nil
}

func globalErrors(value any) []string {
	if value == nil {
		return nil
	}

	global, ok := value.(map[string]any)
	if !ok {
		return []string{"global: must be a mapping"}
	}

	var problems []string
	problems = append(problems, prefixErrors(requiredErrors(global, []string{"max_concurrent_agents", "scheduling"}), "global")...)
	problems = append(problems, positiveIntegerError(global["max_concurrent_agents"], "global.max_concurrent_agents")...)
	problems = append(problems, schedulingErrors(global["scheduling"])...)
	problems = append(problems, optionalMapErrors(global, "identity")...)
	problems = append(problems, identityErrors(global["identity"], "global.identity")...)
	problems = append(problems, optionalMapErrors(global, "fair_share")...)
	problems = append(problems, optionalMapErrors(global, "startup")...)

	if startup, ok := global["startup"].(map[string]any); ok {
		problems = append(problems, startupErrors(startup, "global.startup")...)
	}

	return problems
}

func schedulingErrors(value any) []string {
	if value == nil {
		return nil
	}
	mode, ok := value.(string)
	if ok && validSchedulingMode(mode) {
		return nil
	}
	return []string{"global.scheduling: must be one of " + strings.Join(schedulingModes, ", ")}
}

func validSchedulingMode(mode string) bool {
	for _, candidate := range schedulingModes {
		if mode == candidate {
			return true
		}
	}
	return false
}

func optionalMapErrors(attrs map[string]any, field string) []string {
	value, ok := attrs[field]
	if !ok {
		return nil
	}
	if _, ok := value.(map[string]any); ok {
		return nil
	}
	return []string{"global." + field + ": must be a mapping"}
}

func startupErrors(startup map[string]any, prefix string) []string {
	if startup == nil {
		return nil
	}

	var problems []string
	if value, ok := startup["jitter_seconds"]; ok && !nonNegativeInteger(value) {
		problems = append(problems, prefix+".jitter_seconds: must be an integer greater than or equal to 0")
	}
	if value, ok := startup["max_spawn_per_second"]; ok && !positiveInteger(value) {
		problems = append(problems, prefix+".max_spawn_per_second: must be a positive integer")
	}
	return problems
}

func projectsErrors(value any, opts options) []string {
	if value == nil {
		return nil
	}

	projects, ok := value.([]any)
	if !ok {
		return []string{"projects: must be a list"}
	}

	var problems []string
	for index, project := range projects {
		problems = append(problems, projectErrors(project, index, opts)...)
	}
	problems = append(problems, duplicateProjectIDErrors(projects)...)
	return problems
}

func projectErrors(value any, index int, opts options) []string {
	project, ok := value.(map[string]any)
	prefix := fmt.Sprintf("projects[%d]", index)
	if !ok {
		return []string{prefix + ": must be a mapping"}
	}

	var problems []string
	problems = append(problems, prefixErrors(requiredErrors(project, []string{"id", "workflow", "workdir", "weight", "priority"}), prefix)...)
	problems = append(problems, stringErrors(project, "id", prefix)...)
	problems = append(problems, pathErrors(project, "workflow", prefix, opts, wantFile)...)
	problems = append(problems, pathErrors(project, "workdir", prefix, opts, wantDirectory)...)
	problems = append(problems, positiveIntegerError(project["weight"], prefix+".weight")...)
	problems = append(problems, integerError(project["priority"], prefix+".priority")...)
	problems = append(problems, pausedErrors(project, prefix)...)
	problems = append(problems, credentialRefErrors(project, prefix)...)
	problems = append(problems, authorizationErrors(project["authorization"], prefix+".authorization")...)

	return problems
}

func identityErrors(value any, prefix string) []string {
	if value == nil {
		return nil
	}
	if _, ok := value.(map[string]any); !ok {
		return nil
	}

	var identity Identity
	if err := decodeYAMLValue(value, &identity); err != nil {
		return []string{prefix + ": " + err.Error()}
	}
	identity.Normalize()
	return identity.Validate(prefix)
}

func authorizationErrors(value any, prefix string) []string {
	if value == nil {
		return nil
	}
	if _, ok := value.(map[string]any); !ok {
		return []string{prefix + ": must be a mapping"}
	}

	var authorization selector.Selector
	if err := decodeYAMLValue(value, &authorization); err != nil {
		return []string{prefix + ": " + err.Error()}
	}
	authorization.Normalize()
	return authorization.Validate(prefix)
}

func prefixErrors(errors []string, prefix string) []string {
	out := make([]string, 0, len(errors))
	for _, err := range errors {
		out = append(out, prefix+"."+err)
	}
	return out
}

func stringErrors(attrs map[string]any, field string, prefix string) []string {
	value, ok := attrs[field]
	if !ok || value == nil {
		return nil
	}

	text, ok := value.(string)
	if !ok {
		return []string{prefix + "." + field + ": must be a string"}
	}
	if strings.TrimSpace(text) == "" {
		return []string{prefix + "." + field + ": must not be blank"}
	}
	return nil
}

type pathExpectation int

const (
	wantFile pathExpectation = iota
	wantDirectory
)

func pathErrors(attrs map[string]any, field string, prefix string, opts options, expected pathExpectation) []string {
	value, ok := attrs[field]
	if !ok || value == nil {
		return nil
	}

	text, ok := value.(string)
	if !ok {
		return []string{prefix + "." + field + ": must be a string"}
	}
	return projectPathErrors(text, prefix+"."+field, opts, expected)
}

func projectPathErrors(path string, field string, opts options, expected pathExpectation) []string {
	if strings.TrimSpace(path) == "" {
		return []string{field + ": must not be blank"}
	}

	expanded, err := expandPath(path, opts)
	if err != nil {
		return []string{field + ": path does not exist"}
	}

	info, err := os.Stat(expanded)
	if err != nil {
		return []string{field + ": path does not exist"}
	}
	if expected == wantFile && !info.Mode().IsRegular() {
		return []string{field + ": path does not exist"}
	}
	if expected == wantDirectory && !info.IsDir() {
		return []string{field + ": path does not exist"}
	}
	return nil
}

func positiveIntegerError(value any, field string) []string {
	if value == nil {
		return nil
	}
	if positiveInteger(value) {
		return nil
	}
	return []string{field + ": must be a positive integer"}
}

func integerError(value any, field string) []string {
	if value == nil {
		return nil
	}
	if _, ok := value.(int); ok {
		return nil
	}
	return []string{field + ": must be an integer"}
}

func positiveInteger(value any) bool {
	number, ok := value.(int)
	return ok && number > 0
}

func nonNegativeInteger(value any) bool {
	number, ok := value.(int)
	return ok && number >= 0
}

func pausedErrors(attrs map[string]any, prefix string) []string {
	value, ok := attrs["paused"]
	if !ok {
		return nil
	}
	if _, ok := value.(bool); ok {
		return nil
	}
	return []string{prefix + ".paused: must be a boolean"}
}

func credentialRefErrors(attrs map[string]any, prefix string) []string {
	value, ok := attrs["credential_ref"]
	if !ok || value == nil {
		return nil
	}

	text, ok := value.(string)
	if !ok {
		return []string{prefix + ".credential_ref: must be a string"}
	}
	if strings.TrimSpace(text) == "" {
		return []string{prefix + ".credential_ref: must not be blank"}
	}
	return nil
}

func duplicateProjectIDErrors(projects []any) []string {
	counts := make(map[string]int)
	for _, item := range projects {
		project, ok := item.(map[string]any)
		if !ok {
			continue
		}

		id, ok := project["id"].(string)
		if !ok {
			continue
		}

		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		counts[id]++
	}

	var problems []string
	for id, count := range counts {
		if count > 1 {
			problems = append(problems, "projects.id: duplicate id "+id)
		}
	}
	return problems
}

func duplicateProjectIDErrorsFromProjects(projects []Project) []string {
	counts := make(map[string]int)
	for _, project := range projects {
		id := strings.TrimSpace(project.ID)
		if id == "" {
			continue
		}
		counts[id]++
	}

	var problems []string
	for id, count := range counts {
		if count > 1 {
			problems = append(problems, "projects.id: duplicate id "+id)
		}
	}
	return problems
}

func build(attrs map[string]any, path string, opts options) (Config, error) {
	global, err := mapValue(attrs["global"], "global")
	if err != nil {
		return Config{}, buildValidationError(path, err)
	}
	projects, err := listValue(attrs["projects"], "projects")
	if err != nil {
		return Config{}, buildValidationError(path, err)
	}
	apiVersion, err := stringValue(attrs["apiVersion"], "apiVersion")
	if err != nil {
		return Config{}, buildValidationError(path, err)
	}
	kind, err := stringValue(attrs["kind"], "kind")
	if err != nil {
		return Config{}, buildValidationError(path, err)
	}
	settings, err := buildSettings(global)
	if err != nil {
		return Config{}, buildValidationError(path, err)
	}
	builtProjects, err := buildProjects(projects, opts)
	if err != nil {
		return Config{}, buildValidationError(path, err)
	}

	return Config{
		Path:       path,
		APIVersion: apiVersion,
		Kind:       kind,
		Global:     settings,
		Projects:   builtProjects,
	}, nil
}

func buildSettings(attrs map[string]any) (Settings, error) {
	settings := defaultSettings()
	maxConcurrentAgents, err := intValue(attrs["max_concurrent_agents"], "global.max_concurrent_agents")
	if err != nil {
		return Settings{}, err
	}
	scheduling, err := stringValue(attrs["scheduling"], "global.scheduling")
	if err != nil {
		return Settings{}, err
	}
	fairShare, err := mergeMap(settings.FairShare, attrs["fair_share"], "global.fair_share")
	if err != nil {
		return Settings{}, err
	}
	startup, err := mergeMap(settings.Startup, attrs["startup"], "global.startup")
	if err != nil {
		return Settings{}, err
	}
	identity, err := buildIdentity(attrs["identity"], "global.identity")
	if err != nil {
		return Settings{}, err
	}

	settings.MaxConcurrentAgents = maxConcurrentAgents
	settings.Scheduling = scheduling
	settings.Identity = identity
	settings.FairShare = fairShare
	settings.Startup = startup
	return settings, nil
}

func buildProjects(projects []any, opts options) ([]Project, error) {
	out := make([]Project, 0, len(projects))
	for index, item := range projects {
		prefix := fmt.Sprintf("projects[%d]", index)
		project, err := mapValue(item, prefix)
		if err != nil {
			return nil, err
		}
		workflow, err := stringValue(project["workflow"], prefix+".workflow")
		if err != nil {
			return nil, err
		}
		workdir, err := stringValue(project["workdir"], prefix+".workdir")
		if err != nil {
			return nil, err
		}
		if !opts.projectPathLiterals {
			expandedWorkflow, err := expandPath(workflow, opts)
			if err != nil {
				return nil, fmt.Errorf("%s.workflow: expand path: %w", prefix, err)
			}
			expandedWorkdir, err := expandPath(workdir, opts)
			if err != nil {
				return nil, fmt.Errorf("%s.workdir: expand path: %w", prefix, err)
			}
			workflow = expandedWorkflow
			workdir = expandedWorkdir
		}
		id, err := stringValue(project["id"], prefix+".id")
		if err != nil {
			return nil, err
		}
		weight, err := intValue(project["weight"], prefix+".weight")
		if err != nil {
			return nil, err
		}
		priority, err := intValue(project["priority"], prefix+".priority")
		if err != nil {
			return nil, err
		}
		paused, err := optionalBool(project["paused"], prefix+".paused")
		if err != nil {
			return nil, err
		}
		credentialRef, err := optionalString(project["credential_ref"], prefix+".credential_ref")
		if err != nil {
			return nil, err
		}
		authorization, err := buildAuthorization(project["authorization"], prefix+".authorization")
		if err != nil {
			return nil, err
		}

		out = append(out, Project{
			ID:            strings.TrimSpace(id),
			Workflow:      workflow,
			Workdir:       workdir,
			Weight:        weight,
			Priority:      priority,
			Paused:        paused,
			CredentialRef: credentialRef,
			Authorization: authorization,
		})
	}
	return out, nil
}

func buildIdentity(value any, field string) (Identity, error) {
	var identity Identity
	if value == nil {
		return identity, nil
	}
	if _, err := mapValue(value, field); err != nil {
		return Identity{}, err
	}
	if err := decodeYAMLValue(value, &identity); err != nil {
		return Identity{}, fmt.Errorf("%s: %w", field, err)
	}
	identity.Normalize()
	if problems := identity.Validate(field); len(problems) > 0 {
		return Identity{}, errors.New(strings.Join(problems, "; "))
	}
	return identity, nil
}

func buildAuthorization(value any, field string) (selector.Selector, error) {
	var authorization selector.Selector
	if value == nil {
		return authorization, nil
	}
	if _, err := mapValue(value, field); err != nil {
		return selector.Selector{}, err
	}
	if err := decodeYAMLValue(value, &authorization); err != nil {
		return selector.Selector{}, fmt.Errorf("%s: %w", field, err)
	}
	authorization.Normalize()
	if problems := authorization.Validate(field); len(problems) > 0 {
		return selector.Selector{}, errors.New(strings.Join(problems, "; "))
	}
	return authorization, nil
}

func decodeYAMLValue(value any, out any) error {
	raw, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(raw, out)
}

func mergeMap(defaults map[string]any, value any, field string) (map[string]any, error) {
	out := make(map[string]any, len(defaults))
	for key, nestedValue := range defaults {
		out[key] = nestedValue
	}

	if value == nil {
		return out, nil
	}

	source, err := mapValue(value, field)
	if err != nil {
		return nil, err
	}
	for key, nestedValue := range source {
		out[key] = nestedValue
	}
	return out, nil
}

func optionalBool(value any, field string) (bool, error) {
	if value == nil {
		return false, nil
	}
	paused, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s: must be a boolean", field)
	}
	return paused, nil
}

func optionalString(value any, field string) (string, error) {
	if value == nil {
		return "", nil
	}
	text, err := stringValue(value, field)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(text), nil
}

func mapValue(value any, field string) (map[string]any, error) {
	typed, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be a mapping", field)
	}
	return typed, nil
}

func listValue(value any, field string) ([]any, error) {
	typed, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be a list", field)
	}
	return typed, nil
}

func stringValue(value any, field string) (string, error) {
	typed, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s: must be a string", field)
	}
	return typed, nil
}

func intValue(value any, field string) (int, error) {
	typed, ok := value.(int)
	if !ok {
		return 0, fmt.Errorf("%s: must be an integer", field)
	}
	return typed, nil
}

func buildValidationError(path string, err error) error {
	return ValidationError{Path: path, Problems: []string{err.Error()}}
}

func expandPath(path string, opts options) (string, error) {
	switch {
	case path == "~" || path == "~/":
		if opts.home == "" {
			return "", errors.New("home directory is not available")
		}
		return filepath.Clean(opts.home), nil
	case strings.HasPrefix(path, "~/"):
		if opts.home == "" {
			return "", errors.New("home directory is not available")
		}
		return filepath.Join(opts.home, strings.TrimPrefix(path, "~/")), nil
	case filepath.IsAbs(path):
		return filepath.Clean(path), nil
	default:
		return filepath.Abs(filepath.Join(opts.relativeTo, path))
	}
}

func normalizeYAML(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, nestedValue := range typed {
			out[key] = normalizeYAML(nestedValue)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, nestedValue := range typed {
			out[fmt.Sprint(key)] = normalizeYAML(nestedValue)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, nestedValue := range typed {
			out = append(out, normalizeYAML(nestedValue))
		}
		return out
	default:
		return value
	}
}
