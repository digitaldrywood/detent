package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const projectOptionsQuery = `
query DetentGitHubProjectOptions($projectId: ID!) {
  node(id: $projectId) {
    __typename
    ... on ProjectV2 {
      statusField: field(name: "Status") {
        __typename
        ... on ProjectV2SingleSelectField {
          id
          options { id name color description }
        }
      }
      priorityField: field(name: "Priority") {
        __typename
        ... on ProjectV2SingleSelectField {
          id
          options { id name color description }
        }
      }
    }
  }
}`

const updateProjectFieldMutation = `
mutation DetentGitHubUpdateProjectField($input: UpdateProjectV2FieldInput!) {
  updateProjectV2Field(input: $input) {
    projectV2Field {
      ... on ProjectV2SingleSelectField {
        options { id name color description }
      }
    }
  }
}`

var statusOptionDefaultsByState = map[string]projectSingleSelectOption{
	"Backlog":      {Color: "GRAY", Description: "Not ready for Detent dispatch."},
	"Todo":         {Color: "GRAY", Description: "Ready for Detent dispatch."},
	"In Progress":  {Color: "YELLOW", Description: "Work is currently active."},
	"Merging":      {Color: "PURPLE", Description: "Approved work is being integrated."},
	"Rework":       {Color: "ORANGE", Description: "Changes are requested before review can continue."},
	"Human Review": {Color: "PURPLE", Description: "Waiting for human review."},
	"Blocked":      {Color: "RED", Description: "Cannot continue without human input."},
	"Done":         {Color: "GREEN", Description: "Work is complete."},
	"Closed":       {Color: "GREEN", Description: "Work is closed."},
	"Cancelled":    {Color: "GRAY", Description: "Work will not continue."},
	"Canceled":     {Color: "GRAY", Description: "Work will not continue."},
	"Duplicate":    {Color: "GRAY", Description: "Work is tracked elsewhere."},
}

// GitHub Projects uses single-select option order as kanban column order.
var defaultStatusOptionOrder = []string{
	"Backlog",
	"Todo",
	"In Progress",
	"Blocked",
	"Human Review",
	"Rework",
	"Merging",
	"Done",
	"Closed",
	"Cancelled",
	"Canceled",
	"Duplicate",
}

var defaultPriorityOptionsByName = map[string]projectSingleSelectOption{
	"Urgent":      {Name: "Urgent", Color: "RED", Description: "Needs immediate attention."},
	"High":        {Name: "High", Color: "ORANGE", Description: "Important work to prioritize soon."},
	"Medium":      {Name: "Medium", Color: "YELLOW", Description: "Normal priority work."},
	"Low":         {Name: "Low", Color: "BLUE", Description: "Can wait behind higher-priority work."},
	"No priority": {Name: "No priority", Color: "GRAY", Description: "Priority has not been set."},
}

type projectOptionsMetadata struct {
	StatusField   projectSingleSelectField
	PriorityField *projectSingleSelectField
}

type projectSingleSelectField struct {
	ID      string
	Options []projectSingleSelectOption
}

type projectSingleSelectOption struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type priorityOptionRequirement struct {
	Name string
	Rank *int
}

type statusOptionRequirement struct {
	State       string
	Option      projectSingleSelectOption
	InputOffset int
}

type projectOptionsFieldResponse struct {
	TypeName string                  `json:"__typename"`
	ID       string                  `json:"id"`
	Options  []projectOptionResponse `json:"options"`
}

type projectOptionResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func (c *Connector) Provision(ctx context.Context) error {
	return c.EnsureStateOptions(ctx)
}

func (c *Connector) EnsureStateOptions(ctx context.Context) error {
	if c.projectID == "" {
		return ErrMissingProject
	}

	metadata, err := c.fetchProjectOptionsMetadata(ctx)
	if err != nil {
		return err
	}

	statusCreated, err := c.ensureFieldOptions(ctx, metadata.StatusField, c.requiredStatusOptions())
	if err != nil {
		return fmt.Errorf("ensure github status options: %w", err)
	}

	if metadata.PriorityField != nil {
		_, err = c.ensureFieldOptions(ctx, *metadata.PriorityField, c.requiredPriorityOptions())
		if err != nil {
			return fmt.Errorf("ensure github priority options: %w", err)
		}
	}
	if statusCreated {
		c.statusCache.Clear(c.projectID)
	}

	return nil
}

func (c *Connector) fetchProjectOptionsMetadata(ctx context.Context) (projectOptionsMetadata, error) {
	var response struct {
		Node *struct {
			TypeName      string                       `json:"__typename"`
			StatusField   *projectOptionsFieldResponse `json:"statusField"`
			PriorityField *projectOptionsFieldResponse `json:"priorityField"`
		} `json:"node"`
	}
	if err := c.client.GraphQL(ctx, projectOptionsQuery, map[string]any{"projectId": c.projectID}, &response); err != nil {
		return projectOptionsMetadata{}, fmt.Errorf("fetch github project options: %w", err)
	}
	if response.Node == nil || response.Node.TypeName != "ProjectV2" {
		return projectOptionsMetadata{}, ErrProjectNotFound
	}

	statusField, err := decodeProjectSingleSelectField("Status", response.Node.StatusField)
	if err != nil {
		return projectOptionsMetadata{}, err
	}
	priorityField, err := decodeOptionalProjectSingleSelectField("Priority", response.Node.PriorityField)
	if err != nil {
		return projectOptionsMetadata{}, err
	}

	return projectOptionsMetadata{
		StatusField:   statusField,
		PriorityField: priorityField,
	}, nil
}

func decodeProjectSingleSelectField(fieldName string, field *projectOptionsFieldResponse) (projectSingleSelectField, error) {
	if field == nil {
		return projectSingleSelectField{}, fmt.Errorf("%w: %s", ErrProjectFieldNotFound, fieldName)
	}
	if field.TypeName != "ProjectV2SingleSelectField" || strings.TrimSpace(field.ID) == "" {
		return projectSingleSelectField{}, fmt.Errorf("%w: %s", ErrProjectFieldNotFound, fieldName)
	}

	options := make([]projectSingleSelectOption, 0, len(field.Options))
	for _, option := range field.Options {
		name := strings.TrimSpace(option.Name)
		if name == "" {
			continue
		}
		options = append(options, projectSingleSelectOption{
			ID:          strings.TrimSpace(option.ID),
			Name:        name,
			Color:       strings.TrimSpace(option.Color),
			Description: strings.TrimSpace(option.Description),
		})
	}

	return projectSingleSelectField{
		ID:      strings.TrimSpace(field.ID),
		Options: options,
	}, nil
}

func decodeOptionalProjectSingleSelectField(fieldName string, field *projectOptionsFieldResponse) (*projectSingleSelectField, error) {
	if field == nil {
		return nil, nil
	}
	decoded, err := decodeProjectSingleSelectField(fieldName, field)
	if err != nil {
		return nil, err
	}
	return &decoded, nil
}

func (c *Connector) ensureFieldOptions(ctx context.Context, field projectSingleSelectField, required []projectSingleSelectOption) (bool, error) {
	options := singleSelectOptionsWithRequiredOrder(field.Options, required)
	if singleSelectOptionsEqual(field.Options, options) {
		return false, nil
	}

	if _, err := c.updateProjectFieldOptions(ctx, field.ID, options); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Connector) updateProjectFieldOptions(
	ctx context.Context,
	fieldID string,
	options []projectSingleSelectOption,
) ([]projectSingleSelectOption, error) {
	var response struct {
		UpdateProjectV2Field *struct {
			ProjectV2Field *struct {
				Options []projectSingleSelectOption `json:"options"`
			} `json:"projectV2Field"`
		} `json:"updateProjectV2Field"`
	}
	if err := c.client.GraphQL(ctx, updateProjectFieldMutation, map[string]any{
		"input": map[string]any{
			"fieldId":             fieldID,
			"singleSelectOptions": options,
		},
	}, &response); err != nil {
		return nil, err
	}
	if response.UpdateProjectV2Field == nil ||
		response.UpdateProjectV2Field.ProjectV2Field == nil ||
		response.UpdateProjectV2Field.ProjectV2Field.Options == nil {
		return nil, ErrProjectFieldUpdateFailed
	}

	return response.UpdateProjectV2Field.ProjectV2Field.Options, nil
}

func singleSelectOptionsWithRequiredOrder(current []projectSingleSelectOption, required []projectSingleSelectOption) []projectSingleSelectOption {
	options := make([]projectSingleSelectOption, 0, len(current)+len(required))
	currentByName := make(map[string]projectSingleSelectOption, len(current))
	currentNames := make([]string, 0, len(current))
	for _, option := range current {
		input := singleSelectOptionInput(option)
		if strings.TrimSpace(input.Name) == "" {
			continue
		}
		if _, ok := currentByName[input.Name]; ok {
			continue
		}
		currentByName[input.Name] = input
		currentNames = append(currentNames, input.Name)
	}

	seen := make(map[string]struct{}, len(required))
	for _, option := range required {
		input := singleSelectOptionInput(option)
		if input.Name == "" {
			continue
		}
		if _, ok := seen[input.Name]; ok {
			continue
		}
		seen[input.Name] = struct{}{}
		if existing, ok := currentByName[input.Name]; ok {
			options = append(options, existing)
			continue
		}
		options = append(options, input)
	}

	for _, name := range currentNames {
		if _, ok := seen[name]; ok {
			continue
		}
		options = append(options, currentByName[name])
	}
	return options
}

func singleSelectOptionsEqual(current []projectSingleSelectOption, want []projectSingleSelectOption) bool {
	current = normalizedSingleSelectOptions(current)
	want = normalizedSingleSelectOptions(want)
	if len(current) != len(want) {
		return false
	}
	for i := range current {
		if current[i] != want[i] {
			return false
		}
	}
	return true
}

func normalizedSingleSelectOptions(options []projectSingleSelectOption) []projectSingleSelectOption {
	normalized := make([]projectSingleSelectOption, 0, len(options))
	seen := make(map[string]struct{}, len(options))
	for _, option := range options {
		input := singleSelectOptionInput(option)
		if input.Name == "" {
			continue
		}
		if _, ok := seen[input.Name]; ok {
			continue
		}
		seen[input.Name] = struct{}{}
		normalized = append(normalized, input)
	}
	return normalized
}

func singleSelectOptionInput(option projectSingleSelectOption) projectSingleSelectOption {
	option.ID = strings.TrimSpace(option.ID)
	option.Name = strings.TrimSpace(option.Name)
	option.Color = strings.TrimSpace(option.Color)
	option.Description = strings.TrimSpace(option.Description)
	if option.Color == "" {
		option.Color = "GRAY"
	}
	return option
}

func (c *Connector) requiredStatusOptions() []projectSingleSelectOption {
	requirements := make([]statusOptionRequirement, 0, len(c.activeStates)+len(c.observedStates)+len(c.terminalStates))
	seen := map[string]struct{}{}
	for index, state := range appendStatusStates(nil, c.activeStates, c.observedStates, c.terminalStates) {
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		githubState := c.detentToGitHubState(state)
		if githubState == "" {
			continue
		}
		if _, ok := seen[githubState]; ok {
			continue
		}
		seen[githubState] = struct{}{}

		option := statusOptionDefaults(state)
		option.Name = githubState
		requirements = append(requirements, statusOptionRequirement{
			State:       state,
			Option:      option,
			InputOffset: index,
		})
	}
	sort.SliceStable(requirements, func(i, j int) bool {
		leftRank := statusOptionSortRank(requirements[i])
		rightRank := statusOptionSortRank(requirements[j])
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return requirements[i].InputOffset < requirements[j].InputOffset
	})

	options := make([]projectSingleSelectOption, 0, len(requirements))
	for _, requirement := range requirements {
		options = append(options, requirement.Option)
	}
	return options
}

func appendStatusStates(out []string, groups ...[]string) []string {
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

func statusOptionSortRank(requirement statusOptionRequirement) int {
	if rank, ok := statusOptionOrderRank(requirement.State); ok {
		return rank
	}
	return len(defaultStatusOptionOrder) + requirement.InputOffset
}

func statusOptionOrderRank(state string) (int, bool) {
	state = normalizeStateName(state)
	for rank, orderedState := range defaultStatusOptionOrder {
		if normalizeStateName(orderedState) == state {
			return rank, true
		}
	}
	return 0, false
}

func statusOptionDefaults(state string) projectSingleSelectOption {
	if option, ok := statusOptionDefaultsByState[state]; ok {
		return option
	}
	return projectSingleSelectOption{
		Color:       "GRAY",
		Description: "Detent workflow state.",
	}
}

func (c *Connector) requiredPriorityOptions() []projectSingleSelectOption {
	requirements := make([]priorityOptionRequirement, 0, len(c.priorityMap))
	for name, rank := range c.priorityMap {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		requirements = append(requirements, priorityOptionRequirement{Name: name, Rank: rank})
	}
	sort.Slice(requirements, func(i, j int) bool {
		leftRank := prioritySortRank(requirements[i].Rank)
		rightRank := prioritySortRank(requirements[j].Rank)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return requirements[i].Name < requirements[j].Name
	})

	options := make([]projectSingleSelectOption, 0, len(requirements))
	for _, requirement := range requirements {
		if option, ok := defaultPriorityOptionsByName[requirement.Name]; ok {
			options = append(options, option)
			continue
		}
		options = append(options, projectSingleSelectOption{
			Name:        requirement.Name,
			Color:       priorityColor(requirement.Rank),
			Description: priorityDescription(requirement.Rank),
		})
	}
	return options
}

func prioritySortRank(rank *int) int {
	if rank == nil {
		return 99
	}
	return *rank
}

func priorityColor(rank *int) string {
	if rank == nil {
		return "GRAY"
	}
	switch *rank {
	case 1:
		return "RED"
	case 2:
		return "ORANGE"
	case 3:
		return "YELLOW"
	case 4:
		return "BLUE"
	default:
		return "GRAY"
	}
}

func priorityDescription(rank *int) string {
	if rank == nil {
		return "Unranked Detent priority."
	}
	return fmt.Sprintf("Detent priority rank %d.", *rank)
}
