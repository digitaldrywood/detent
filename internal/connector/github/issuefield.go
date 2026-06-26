package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

const issueSearchPageSize = 100

type issueFieldCache struct {
	mu      sync.RWMutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]issueFieldCacheEntry
}

type issueFieldCacheEntry struct {
	metadata issueFieldMetadata
	cachedAt time.Time
}

type issueFieldMetadata struct {
	Org           string
	FieldID       int
	NodeID        string
	Name          string
	DataType      string
	OptionsByName map[string]issueFieldOption
}

type issueFieldOption struct {
	ID          int
	Name        string
	Color       string
	Description string
	Priority    int
}

type restIssueField struct {
	ID          int                    `json:"id"`
	NodeID      string                 `json:"node_id"`
	Name        string                 `json:"name"`
	DataType    string                 `json:"data_type"`
	Description string                 `json:"description"`
	Options     []restIssueFieldOption `json:"options"`
}

type restIssueFieldOption struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Priority    int    `json:"priority"`
}

type restIssueFieldValue struct {
	IssueFieldID       int                    `json:"issue_field_id"`
	NodeID             string                 `json:"node_id"`
	DataType           string                 `json:"data_type"`
	Value              json.RawMessage        `json:"value"`
	SingleSelectOption *restIssueFieldOption  `json:"single_select_option"`
	MultiSelectOptions []restIssueFieldOption `json:"multi_select_options"`
}

type restAuthenticatedUser struct {
	Login string `json:"login"`
}

func newIssueFieldCache(ttl time.Duration, now func() time.Time) *issueFieldCache {
	if now == nil {
		now = time.Now
	}
	return &issueFieldCache{
		ttl:     ttl,
		now:     now,
		entries: map[string]issueFieldCacheEntry{},
	}
}

func (c *issueFieldCache) Get(org string, fieldName string) (issueFieldMetadata, bool) {
	key := issueFieldCacheKey(org, fieldName)
	if key == "" {
		return issueFieldMetadata{}, false
	}

	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return issueFieldMetadata{}, false
	}
	if c.fresh(entry.cachedAt) {
		return cloneIssueFieldMetadata(entry.metadata), true
	}

	c.mu.Lock()
	if current, ok := c.entries[key]; ok && c.fresh(current.cachedAt) {
		entry = current
	} else if ok {
		delete(c.entries, key)
	}
	c.mu.Unlock()

	if c.fresh(entry.cachedAt) {
		return cloneIssueFieldMetadata(entry.metadata), true
	}
	return issueFieldMetadata{}, false
}

func (c *issueFieldCache) Set(metadata issueFieldMetadata) {
	key := issueFieldCacheKey(metadata.Org, metadata.Name)
	if key == "" {
		return
	}

	c.mu.Lock()
	c.entries[key] = issueFieldCacheEntry{
		metadata: cloneIssueFieldMetadata(metadata),
		cachedAt: c.now(),
	}
	c.mu.Unlock()
}

func (c *issueFieldCache) GetByID(org string, fieldID int) (issueFieldMetadata, bool) {
	org = strings.TrimSpace(org)
	if org == "" || fieldID <= 0 {
		return issueFieldMetadata{}, false
	}

	c.mu.RLock()
	for _, entry := range c.entries {
		if !c.fresh(entry.cachedAt) {
			continue
		}
		if strings.EqualFold(entry.metadata.Org, org) && entry.metadata.FieldID == fieldID {
			c.mu.RUnlock()
			return cloneIssueFieldMetadata(entry.metadata), true
		}
	}
	c.mu.RUnlock()

	return issueFieldMetadata{}, false
}

func (c *issueFieldCache) Clear(org string, fieldName string) {
	key := issueFieldCacheKey(org, fieldName)
	if key == "" {
		return
	}

	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

func (c *issueFieldCache) fresh(cachedAt time.Time) bool {
	return c.ttl > 0 && c.now().Sub(cachedAt) < c.ttl
}

func issueFieldCacheKey(org string, fieldName string) string {
	org = strings.ToLower(strings.TrimSpace(org))
	fieldName = strings.ToLower(strings.TrimSpace(fieldName))
	if org == "" || fieldName == "" {
		return ""
	}
	return org + "\x00" + fieldName
}

func cloneIssueFieldMetadata(metadata issueFieldMetadata) issueFieldMetadata {
	return issueFieldMetadata{
		Org:           metadata.Org,
		FieldID:       metadata.FieldID,
		NodeID:        metadata.NodeID,
		Name:          metadata.Name,
		DataType:      metadata.DataType,
		OptionsByName: cloneIssueFieldOptions(metadata.OptionsByName),
	}
}

func cloneIssueFieldOptions(values map[string]issueFieldOption) map[string]issueFieldOption {
	if values == nil {
		return nil
	}

	cloned := make(map[string]issueFieldOption, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func (c *Connector) authenticateIssueField(ctx context.Context) error {
	if !validPullRequestRepo(c.repository) {
		return ErrMissingRepository
	}

	var user restAuthenticatedUser
	if err := c.client.REST(ctx, http.MethodGet, "/user", nil, &user); err != nil {
		return fmt.Errorf("authenticate github connector: %w", err)
	}
	if strings.TrimSpace(user.Login) == "" {
		return ErrAuthenticationFailed
	}

	c.mu.Lock()
	c.instanceLogin = strings.TrimSpace(user.Login)
	c.mu.Unlock()
	return nil
}

func (c *Connector) fetchIssueFieldIssuesByStates(
	ctx context.Context,
	stateNames []string,
	limit int,
) ([]connector.Issue, error) {
	if !validPullRequestRepo(c.repository) {
		return nil, ErrMissingRepository
	}
	wantedStates := normalizedStateSet(stateNames)
	if len(wantedStates) == 0 {
		return []connector.Issue{}, nil
	}
	githubStates := c.detentToGitHubStates(stateNames)
	if len(githubStates) == 0 {
		return []connector.Issue{}, nil
	}
	if err := c.verifyIssueFieldStatusOptions(ctx, stateNames); err != nil {
		return nil, err
	}

	allIssues := []connector.Issue{}
	for page := 1; ; page++ {
		var response restIssueSearchResponse
		if err := c.client.REST(ctx, http.MethodGet, restIssueFieldSearchPath(c.repository, c.statusField, githubStates, page), nil, &response); err != nil {
			return nil, fmt.Errorf("search github issue field values: %w", err)
		}
		for _, item := range response.Items {
			ref, ok := issueRefFromRESTSearchItem(item, issueRef{Owner: c.repository.Owner, Name: c.repository.Name})
			if !ok {
				continue
			}
			issue, ok, err := c.fetchIssueFieldIssueFromREST(ctx, ref, item)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if _, ok := wantedStates[normalizeStateName(issue.State)]; !ok {
				continue
			}
			allIssues = append(allIssues, issue)
			if limit > 0 && len(allIssues) >= limit {
				resolveBlockedByProjectState(allIssues)
				return allIssues, nil
			}
		}
		if len(response.Items) == 0 || page*issueSearchPageSize >= response.TotalCount {
			resolveBlockedByProjectState(allIssues)
			return allIssues, nil
		}
	}
}

func (c *Connector) fetchIssueFieldIssueByRef(ctx context.Context, ref issueRef) (connector.Issue, bool, error) {
	issue, err := c.fetchRESTIssue(ctx, ref)
	if err != nil {
		return connector.Issue{}, false, err
	}
	if strings.TrimSpace(issue.ID) == "" {
		return connector.Issue{}, false, nil
	}
	return c.fetchIssueFieldIssueFromNode(ctx, ref, issue)
}

func (c *Connector) fetchIssueFieldIssueFromREST(ctx context.Context, ref issueRef, issue restIssue) (connector.Issue, bool, error) {
	node := githubIssueNodeFromREST(ref, issue)
	if strings.TrimSpace(node.ID) == "" {
		return connector.Issue{}, false, nil
	}
	return c.fetchIssueFieldIssueFromNode(ctx, ref, node)
}

func (c *Connector) fetchIssueFieldIssueFromNode(ctx context.Context, ref issueRef, issue githubIssueNode) (connector.Issue, bool, error) {
	c.cacheIssueRef(issue)
	fields, err := c.fetchIssueFieldValues(ctx, ref)
	if err != nil {
		return connector.Issue{}, false, err
	}
	stateName := strings.TrimSpace(fields[c.statusField])
	if stateName == "" {
		stateName = c.githubIssueStateToDetentState(issue.State)
	}
	priorityName := strings.TrimSpace(fields["Priority"])
	return c.buildIssue(issue, stateName, priorityName, nil, fields), true, nil
}

func (c *Connector) fetchIssueFieldValues(ctx context.Context, ref issueRef) (map[string]string, error) {
	values, err := fetchRESTList[restIssueFieldValue](ctx, c.client, restIssueFieldValuesListPath(ref))
	if err != nil {
		return nil, fmt.Errorf("fetch github issue field values: %w", err)
	}
	fields := make(map[string]string, len(values))
	for _, value := range values {
		metadata, ok, err := c.issueFieldMetadataByID(ctx, ref.Owner, value.IssueFieldID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		fieldValue := restIssueFieldValueString(value)
		if fieldValue == "" {
			continue
		}
		fields[metadata.Name] = fieldValue
	}
	return fields, nil
}

func (c *Connector) fetchIssueFieldStatus(ctx context.Context, ref issueRef) (string, error) {
	fields, err := c.fetchIssueFieldValues(ctx, ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(fields[c.statusField]), nil
}

func (c *Connector) resolveIssueFieldMetadata(ctx context.Context, org string, fieldName string) (issueFieldMetadata, error) {
	org = strings.TrimSpace(org)
	fieldName = strings.TrimSpace(fieldName)
	if org == "" {
		return issueFieldMetadata{}, ErrMissingRepository
	}
	if fieldName == "" {
		fieldName = defaultGitHubIssueStatusField
	}
	if metadata, ok := c.issueFields.Get(org, fieldName); ok {
		return metadata, nil
	}

	fields, err := fetchRESTList[restIssueField](ctx, c.client, restOrgIssueFieldsListPath(org))
	if err != nil {
		return issueFieldMetadata{}, fmt.Errorf("fetch github issue fields: %w", err)
	}
	var matched issueFieldMetadata
	matchedOK := false
	for _, field := range fields {
		metadata, ok := issueFieldMetadataFromREST(org, field)
		if !ok {
			continue
		}
		c.issueFields.Set(metadata)
		if strings.EqualFold(metadata.Name, fieldName) {
			matched = metadata
			matchedOK = true
		}
	}
	if matchedOK {
		return matched, nil
	}
	if strings.EqualFold(fieldName, c.statusField) {
		return issueFieldMetadata{}, ErrStatusFieldNotFound
	}
	return issueFieldMetadata{}, fmt.Errorf("%w: %s", ErrProjectFieldNotFound, fieldName)
}

func (c *Connector) issueFieldMetadataByID(ctx context.Context, org string, fieldID int) (issueFieldMetadata, bool, error) {
	if fieldID <= 0 {
		return issueFieldMetadata{}, false, nil
	}
	if metadata, ok := c.issueFields.GetByID(org, fieldID); ok {
		return metadata, true, nil
	}
	fields, err := fetchRESTList[restIssueField](ctx, c.client, restOrgIssueFieldsListPath(org))
	if err != nil {
		return issueFieldMetadata{}, false, fmt.Errorf("fetch github issue fields: %w", err)
	}
	for _, field := range fields {
		metadata, ok := issueFieldMetadataFromREST(org, field)
		if !ok {
			continue
		}
		c.issueFields.Set(metadata)
		if metadata.FieldID == fieldID {
			return metadata, true, nil
		}
	}
	return issueFieldMetadata{}, false, nil
}

func (c *Connector) resolveIssueStatusMetadata(ctx context.Context) (issueFieldMetadata, error) {
	if !validPullRequestRepo(c.repository) {
		return issueFieldMetadata{}, ErrMissingRepository
	}
	metadata, err := c.resolveIssueFieldMetadata(ctx, c.repository.Owner, c.statusField)
	if err != nil {
		return issueFieldMetadata{}, err
	}
	if metadata.DataType != "single_select" {
		return issueFieldMetadata{}, ErrStatusFieldNotFound
	}
	return metadata, nil
}

func (c *Connector) verifyIssueFieldStatusOptions(ctx context.Context, stateNames []string) error {
	metadata, err := c.resolveIssueStatusMetadata(ctx)
	if err != nil {
		return err
	}
	seen := map[string]struct{}{}
	for _, stateName := range stateNames {
		stateName = strings.TrimSpace(stateName)
		if stateName == "" {
			continue
		}
		githubState := c.detentToGitHubState(stateName)
		key := normalizeStateName(githubState)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if _, ok := metadata.OptionsByName[githubState]; !ok {
			return fmt.Errorf("%w: %s maps to %s", ErrStatusOptionNotFound, stateName, githubState)
		}
	}
	return nil
}

func (c *Connector) setIssueStatusField(ctx context.Context, ref issueRef, githubState string) error {
	metadata, err := c.resolveIssueStatusMetadata(ctx)
	if err != nil {
		return err
	}
	if _, ok := metadata.OptionsByName[githubState]; !ok {
		c.issueFields.Clear(metadata.Org, metadata.Name)
		metadata, err = c.resolveIssueStatusMetadata(ctx)
		if err != nil {
			return err
		}
		if _, ok := metadata.OptionsByName[githubState]; !ok {
			return fmt.Errorf("%w: %s", ErrStatusOptionNotFound, githubState)
		}
	}
	return c.setIssueFieldValue(ctx, ref, metadata, githubState, ErrStatusUpdateFailed)
}

func (c *Connector) setIssueFieldValueByName(ctx context.Context, issueID string, fieldName string, value string) error {
	ref, ok, err := c.issueRefForID(ctx, issueID, graphQLQueryIssueLookup)
	if err != nil {
		return err
	}
	if !ok {
		return ErrProjectFieldUpdateFailed
	}
	metadata, err := c.resolveIssueFieldMetadata(ctx, ref.Owner, fieldName)
	if err != nil {
		return err
	}
	return c.setIssueFieldValue(ctx, ref, metadata, strings.TrimSpace(value), ErrProjectFieldUpdateFailed)
}

func (c *Connector) setIssueFieldValue(ctx context.Context, ref issueRef, metadata issueFieldMetadata, value string, emptyResponseError error) error {
	input, err := issueFieldValueInput(metadata, value)
	if err != nil {
		return err
	}
	var response []restIssueFieldValue
	c.writeMu.Lock()
	err = c.client.REST(ctx, http.MethodPost, restIssueFieldValuesPath(ref), map[string]any{
		"issue_field_values": []map[string]any{input},
	}, &response)
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("update github issue field: %w", err)
	}
	if len(response) == 0 {
		return emptyResponseError
	}
	return nil
}

func (c *Connector) clearIssueStatusField(ctx context.Context, ref issueRef) error {
	metadata, err := c.resolveIssueStatusMetadata(ctx)
	if err != nil {
		return err
	}
	return c.deleteIssueFieldValue(ctx, ref, metadata.FieldID, ErrStatusUpdateFailed)
}

func (c *Connector) deleteIssueFieldValue(ctx context.Context, ref issueRef, fieldID int, emptyResponseError error) error {
	if fieldID <= 0 {
		return emptyResponseError
	}

	c.writeMu.Lock()
	err := c.client.REST(ctx, http.MethodDelete, restIssueFieldValuePath(ref, fieldID), nil, nil)
	c.writeMu.Unlock()
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("delete github issue field: %w", err)
	}
	return nil
}

func (c *Connector) createIssueStatusField(ctx context.Context, org string, required []projectSingleSelectOption) (issueFieldMetadata, error) {
	options := issueFieldOptionsWithRequiredOrder(nil, required)
	var response restIssueField
	c.writeMu.Lock()
	err := c.client.REST(ctx, http.MethodPost, restOrgIssueFieldsPath(org), map[string]any{
		"name":        c.statusField,
		"description": "Detent workflow state.",
		"data_type":   "single_select",
		"options":     issueFieldOptionInputs(options),
	}, &response)
	c.writeMu.Unlock()
	if err != nil {
		return issueFieldMetadata{}, fmt.Errorf("create github issue field: %w", err)
	}
	metadata, ok := issueFieldMetadataFromREST(org, response)
	if !ok {
		return issueFieldMetadata{}, ErrStatusFieldNotFound
	}
	c.issueFields.Set(metadata)
	return metadata, nil
}

func (c *Connector) updateIssueFieldOptions(ctx context.Context, org string, fieldID int, options []issueFieldOption) (issueFieldMetadata, error) {
	var response restIssueField
	c.writeMu.Lock()
	err := c.client.REST(ctx, http.MethodPatch, restOrgIssueFieldPath(org, fieldID), map[string]any{
		"options": issueFieldOptionInputs(options),
	}, &response)
	c.writeMu.Unlock()
	if err != nil {
		return issueFieldMetadata{}, err
	}
	metadata, ok := issueFieldMetadataFromREST(org, response)
	if !ok {
		return issueFieldMetadata{}, ErrStatusFieldNotFound
	}
	c.issueFields.Set(metadata)
	return metadata, nil
}

func issueFieldOptionsWithRequiredOrder(current map[string]issueFieldOption, required []projectSingleSelectOption) []issueFieldOption {
	currentByName := make(map[string]issueFieldOption, len(current))
	currentNames := make([]string, 0, len(current))
	for _, option := range current {
		input := issueFieldOptionInput(option)
		if strings.TrimSpace(input.Name) == "" {
			continue
		}
		if _, ok := currentByName[input.Name]; ok {
			continue
		}
		currentByName[input.Name] = input
		currentNames = append(currentNames, input.Name)
	}
	sort.Strings(currentNames)

	options := make([]issueFieldOption, 0, len(current)+len(required))
	seen := map[string]struct{}{}
	for index, option := range required {
		input := issueFieldOptionFromProjectOption(option, index+1)
		if input.Name == "" {
			continue
		}
		if _, ok := seen[input.Name]; ok {
			continue
		}
		seen[input.Name] = struct{}{}
		if existing, ok := currentByName[input.Name]; ok {
			existing.Priority = len(options) + 1
			options = append(options, existing)
			continue
		}
		input.Priority = len(options) + 1
		options = append(options, input)
	}

	for _, name := range currentNames {
		if _, ok := seen[name]; ok {
			continue
		}
		option := currentByName[name]
		option.Priority = len(options) + 1
		options = append(options, option)
	}
	return options
}

func issueFieldOptionsEqual(current map[string]issueFieldOption, want []issueFieldOption) bool {
	currentOptions := issueFieldOptionsWithRequiredOrder(current, nil)
	if len(currentOptions) != len(want) {
		return false
	}
	for i := range currentOptions {
		left := issueFieldOptionInput(currentOptions[i])
		right := issueFieldOptionInput(want[i])
		if left != right {
			return false
		}
	}
	return true
}

func issueFieldOptionFromProjectOption(option projectSingleSelectOption, priority int) issueFieldOption {
	option = singleSelectOptionInput(option)
	return issueFieldOption{
		Name:        option.Name,
		Color:       strings.ToLower(option.Color),
		Description: option.Description,
		Priority:    priority,
	}
}

func issueFieldOptionInput(option issueFieldOption) issueFieldOption {
	option.Name = strings.TrimSpace(option.Name)
	option.Color = strings.ToLower(strings.TrimSpace(option.Color))
	option.Description = strings.TrimSpace(option.Description)
	if option.Color == "" {
		option.Color = "gray"
	}
	if option.Priority <= 0 {
		option.Priority = 1
	}
	return option
}

func issueFieldOptionInputs(options []issueFieldOption) []map[string]any {
	inputs := make([]map[string]any, 0, len(options))
	for index, option := range options {
		input := issueFieldOptionInput(option)
		if input.Name == "" {
			continue
		}
		if input.Priority <= 0 {
			input.Priority = index + 1
		}
		value := map[string]any{
			"name":        input.Name,
			"color":       input.Color,
			"description": input.Description,
			"priority":    input.Priority,
		}
		if input.ID > 0 {
			value["id"] = input.ID
		}
		inputs = append(inputs, value)
	}
	return inputs
}

func issueFieldValueInput(metadata issueFieldMetadata, value string) (map[string]any, error) {
	value = strings.TrimSpace(value)
	if metadata.FieldID <= 0 || value == "" {
		return nil, ErrProjectFieldUpdateFailed
	}
	switch metadata.DataType {
	case "single_select":
		if _, ok := metadata.OptionsByName[value]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrProjectFieldOptionNotFound, value)
		}
		return map[string]any{"field_id": metadata.FieldID, "value": value}, nil
	case "number":
		number, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrProjectFieldUpdateFailed, value)
		}
		return map[string]any{"field_id": metadata.FieldID, "value": number}, nil
	default:
		return map[string]any{"field_id": metadata.FieldID, "value": value}, nil
	}
}

func issueFieldMetadataFromREST(org string, field restIssueField) (issueFieldMetadata, bool) {
	name := strings.TrimSpace(field.Name)
	dataType := strings.ToLower(strings.TrimSpace(field.DataType))
	if strings.TrimSpace(org) == "" || field.ID <= 0 || name == "" || dataType == "" {
		return issueFieldMetadata{}, false
	}
	options := make(map[string]issueFieldOption, len(field.Options))
	for _, option := range field.Options {
		name := strings.TrimSpace(option.Name)
		if name == "" {
			continue
		}
		options[name] = issueFieldOption{
			ID:          option.ID,
			Name:        name,
			Color:       strings.ToLower(strings.TrimSpace(option.Color)),
			Description: strings.TrimSpace(option.Description),
			Priority:    option.Priority,
		}
	}
	return issueFieldMetadata{
		Org:           strings.TrimSpace(org),
		FieldID:       field.ID,
		NodeID:        strings.TrimSpace(field.NodeID),
		Name:          name,
		DataType:      dataType,
		OptionsByName: options,
	}, true
}

func restIssueFieldValueString(value restIssueFieldValue) string {
	dataType := strings.ToLower(strings.TrimSpace(value.DataType))
	switch dataType {
	case "single_select":
		if value.SingleSelectOption == nil {
			return ""
		}
		return strings.TrimSpace(value.SingleSelectOption.Name)
	case "multi_select":
		values := make([]string, 0, len(value.MultiSelectOptions))
		for _, option := range value.MultiSelectOptions {
			if name := strings.TrimSpace(option.Name); name != "" {
				values = append(values, name)
			}
		}
		return strings.Join(values, ",")
	default:
		var text string
		if err := json.Unmarshal(value.Value, &text); err == nil {
			return strings.TrimSpace(text)
		}
		var number float64
		if err := json.Unmarshal(value.Value, &number); err == nil {
			return strconv.FormatFloat(number, 'f', -1, 64)
		}
	}
	return ""
}

func restIssueFieldSearchPath(repo pullRequestRepo, fieldName string, values []string, page int) string {
	params := url.Values{}
	query := strings.Join([]string{
		"repo:" + repo.Owner + "/" + repo.Name,
		"is:issue",
		issueFieldSearchQualifier(fieldName, values),
	}, " ")
	params.Set("q", query)
	params.Set("per_page", strconv.Itoa(issueSearchPageSize))
	params.Set("page", strconv.Itoa(page))
	return "/search/issues?" + params.Encode()
}

func issueFieldSearchQualifier(fieldName string, values []string) string {
	quotedValues := make([]string, 0, len(values))
	for _, value := range values {
		if token := githubSearchToken(value); token != "" {
			quotedValues = append(quotedValues, token)
		}
	}
	return "field." + githubSearchToken(fieldName) + ":" + strings.Join(quotedValues, ",")
}

func githubSearchToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '_' || r == '-' || r == '.' {
			continue
		}
		return strconv.Quote(value)
	}
	return value
}

func restOrgIssueFieldsListPath(org string) string {
	values := url.Values{}
	values.Set("per_page", "100")
	return "/orgs/" + url.PathEscape(org) + "/issue-fields?" + values.Encode()
}

func restOrgIssueFieldsPath(org string) string {
	return "/orgs/" + url.PathEscape(org) + "/issue-fields"
}

func restOrgIssueFieldPath(org string, fieldID int) string {
	return restOrgIssueFieldsPath(org) + "/" + strconv.Itoa(fieldID)
}

func restIssueFieldValuesPath(ref issueRef) string {
	return restIssuePath(ref) + "/issue-field-values"
}

func restIssueFieldValuePath(ref issueRef, fieldID int) string {
	return restIssueFieldValuesPath(ref) + "/" + strconv.Itoa(fieldID)
}

func restIssueFieldValuesListPath(ref issueRef) string {
	return restIssueFieldValuesPath(ref) + "?per_page=100"
}

func pullRequestRepoFromName(name string) (pullRequestRepo, bool) {
	owner, repo, ok := splitRepositoryName(name)
	if !ok {
		return pullRequestRepo{}, false
	}
	return pullRequestRepo{Owner: owner, Name: repo}, true
}

func validPullRequestRepo(repo pullRequestRepo) bool {
	return strings.TrimSpace(repo.Owner) != "" && strings.TrimSpace(repo.Name) != ""
}
