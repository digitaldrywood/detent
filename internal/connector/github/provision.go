package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const projectOptionsQuery = `
query SymphonyGitHubProjectOptions($projectId: ID!) {
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
mutation SymphonyGitHubUpdateProjectField($input: UpdateProjectV2FieldInput!) {
  updateProjectV2Field(input: $input) {
    projectV2Field {
      ... on ProjectV2SingleSelectField {
        options { id name color description }
      }
    }
  }
}`

var statusOptionDefaultsByState = map[string]projectSingleSelectOption{
	"Backlog":      {Color: "GRAY", Description: "Not ready for Symphony dispatch."},
	"Todo":         {Color: "GRAY", Description: "Ready for Symphony dispatch."},
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

var defaultPriorityOptionsByName = map[string]projectSingleSelectOption{
	"Urgent":      {Name: "Urgent", Color: "RED", Description: "Needs immediate attention."},
	"High":        {Name: "High", Color: "ORANGE", Description: "Important work to prioritize soon."},
	"Medium":      {Name: "Medium", Color: "YELLOW", Description: "Normal priority work."},
	"Low":         {Name: "Low", Color: "BLUE", Description: "Can wait behind higher-priority work."},
	"No priority": {Name: "No priority", Color: "GRAY", Description: "Priority has not been set."},
}

type projectOptionsMetadata struct {
	StatusField   projectSingleSelectField
	PriorityField projectSingleSelectField
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

	_, err = c.ensureFieldOptions(ctx, metadata.PriorityField, c.requiredPriorityOptions())
	if err != nil {
		return fmt.Errorf("ensure github priority options: %w", err)
	}
	if statusCreated {
		c.statusCache.Clear(c.projectID)
	}

	return nil
}

func (c *Connector) fetchProjectOptionsMetadata(ctx context.Context) (projectOptionsMetadata, error) {
	var response struct {
		Node *struct {
			TypeName    string `json:"__typename"`
			StatusField *struct {
				TypeName string `json:"__typename"`
				ID       string `json:"id"`
				Options  []struct {
					ID          string `json:"id"`
					Name        string `json:"name"`
					Color       string `json:"color"`
					Description string `json:"description"`
				} `json:"options"`
			} `json:"statusField"`
			PriorityField *struct {
				TypeName string `json:"__typename"`
				ID       string `json:"id"`
				Options  []struct {
					ID          string `json:"id"`
					Name        string `json:"name"`
					Color       string `json:"color"`
					Description string `json:"description"`
				} `json:"options"`
			} `json:"priorityField"`
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
	priorityField, err := decodeProjectSingleSelectField("Priority", response.Node.PriorityField)
	if err != nil {
		return projectOptionsMetadata{}, err
	}

	return projectOptionsMetadata{
		StatusField:   statusField,
		PriorityField: priorityField,
	}, nil
}

func decodeProjectSingleSelectField(fieldName string, field *struct {
	TypeName string `json:"__typename"`
	ID       string `json:"id"`
	Options  []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Color       string `json:"color"`
		Description string `json:"description"`
	} `json:"options"`
}) (projectSingleSelectField, error) {
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

func (c *Connector) ensureFieldOptions(ctx context.Context, field projectSingleSelectField, required []projectSingleSelectOption) (bool, error) {
	missing := missingProjectOptions(field.Options, required)
	if len(missing) == 0 {
		return false, nil
	}

	options := singleSelectOptionsWithAppended(field.Options, missing)
	var response struct {
		UpdateProjectV2Field *struct {
			ProjectV2Field *struct {
				Options []projectSingleSelectOption `json:"options"`
			} `json:"projectV2Field"`
		} `json:"updateProjectV2Field"`
	}
	if err := c.client.GraphQL(ctx, updateProjectFieldMutation, map[string]any{
		"input": map[string]any{
			"fieldId":             field.ID,
			"singleSelectOptions": options,
		},
	}, &response); err != nil {
		return false, err
	}
	if response.UpdateProjectV2Field == nil ||
		response.UpdateProjectV2Field.ProjectV2Field == nil ||
		response.UpdateProjectV2Field.ProjectV2Field.Options == nil {
		return false, ErrProjectFieldUpdateFailed
	}

	return true, nil
}

func missingProjectOptions(current []projectSingleSelectOption, required []projectSingleSelectOption) []projectSingleSelectOption {
	currentNames := make(map[string]struct{}, len(current))
	for _, option := range current {
		name := strings.TrimSpace(option.Name)
		if name != "" {
			currentNames[name] = struct{}{}
		}
	}

	missing := make([]projectSingleSelectOption, 0, len(required))
	for _, option := range required {
		name := strings.TrimSpace(option.Name)
		if name == "" {
			continue
		}
		if _, ok := currentNames[name]; ok {
			continue
		}
		currentNames[name] = struct{}{}
		option.Name = name
		missing = append(missing, option)
	}
	return missing
}

func singleSelectOptionsWithAppended(current []projectSingleSelectOption, newOptions []projectSingleSelectOption) []projectSingleSelectOption {
	options := make([]projectSingleSelectOption, 0, len(current)+len(newOptions))
	currentNames := make(map[string]struct{}, len(current))
	for _, option := range current {
		input := singleSelectOptionInput(option)
		if strings.TrimSpace(input.Name) == "" {
			continue
		}
		currentNames[input.Name] = struct{}{}
		options = append(options, input)
	}
	for _, option := range newOptions {
		input := singleSelectOptionInput(option)
		if _, ok := currentNames[input.Name]; ok {
			continue
		}
		options = append(options, input)
	}
	return options
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
	states := make([]string, 0, len(c.activeStates)+len(c.observedStates)+len(c.terminalStates))
	states = append(states, c.activeStates...)
	states = append(states, c.observedStates...)
	states = append(states, c.terminalStates...)

	options := make([]projectSingleSelectOption, 0, len(states))
	seen := map[string]struct{}{}
	for _, state := range states {
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		githubState := c.symphonyToGitHubState(state)
		if githubState == "" {
			continue
		}
		if _, ok := seen[githubState]; ok {
			continue
		}
		seen[githubState] = struct{}{}

		option := statusOptionDefaults(state)
		option.Name = githubState
		options = append(options, option)
	}
	return options
}

func statusOptionDefaults(state string) projectSingleSelectOption {
	if option, ok := statusOptionDefaultsByState[state]; ok {
		return option
	}
	return projectSingleSelectOption{
		Color:       "GRAY",
		Description: "Symphony workflow state.",
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
		return "Unranked Symphony priority."
	}
	return fmt.Sprintf("Symphony priority rank %d.", *rank)
}
