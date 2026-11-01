package storagecatalog

import "strings"

type Source struct {
	Schema         string
	Store          string
	CatalogVersion string
	OrganizationID string
	Organizations  []string
}

type Binding struct {
	catalog       Catalog
	source        Source
	organizations map[string]struct{}
}

func (c Catalog) Bind(source Source, schema Schema) (*Binding, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if !identifier(source.Schema) || source.CatalogVersion != c.Version || len(schema) == 0 {
		return nil, ErrUnmapped
	}
	if err := c.ValidateSchema(source.Store, schema); err != nil {
		return nil, err
	}
	organizations := make(map[string]struct{}, len(source.Organizations))
	if source.Store == "hub" || source.Store == "metrics" {
		if source.OrganizationID == "" || len(source.Organizations) != 0 {
			return nil, ErrOrganization
		}
	} else {
		if source.OrganizationID != "" || len(source.Organizations) == 0 {
			return nil, ErrOrganization
		}
		for _, organization := range source.Organizations {
			_, duplicate := organizations[organization]
			if organization == "" || strings.TrimSpace(organization) != organization || duplicate {
				return nil, ErrOrganization
			}
			organizations[organization] = struct{}{}
		}
	}
	if strings.TrimSpace(source.OrganizationID) != source.OrganizationID {
		return nil, ErrOrganization
	}
	source.Organizations = nil
	return &Binding{catalog: c, source: source, organizations: organizations}, nil
}

func (b *Binding) ValidateRow(tableName string, row map[string]Cell) (string, error) {
	table, ok := b.catalog.Tables[b.source.Store+"/"+tableName]
	if !ok || len(row) != len(table.Columns) {
		return "", ErrUnmapped
	}
	for name := range row {
		if _, ok := table.Columns[name]; !ok {
			return "", ErrUnmapped
		}
	}
	organization := b.source.OrganizationID
	organizationColumn := "organization_id"
	if b.source.Store == "registry" && tableName == "organizations" {
		organizationColumn = "id"
	}
	if cell, ok := row[organizationColumn]; ok {
		if cell.Null || cell.Encoding != "utf8" {
			return "", ErrOrganization
		}
		fromRow := string(cell.Data)
		if b.source.OrganizationID != "" {
			if fromRow != b.source.OrganizationID {
				return "", ErrOrganization
			}
		} else if fromRow != "" {
			if _, allowed := b.organizations[fromRow]; !allowed {
				return "", ErrOrganization
			}
			organization = fromRow
		}
	}
	if table.Organization == "row" && organization == "" {
		return "", ErrOrganization
	}
	for name, column := range table.Columns {
		for _, target := range column.Targets {
			if target.Column != "organization_id" && target.Column != "service_organization_id" {
				continue
			}
			cell := row[name]
			if cell.Null || len(cell.Data) == 0 {
				continue
			}
			if cell.Encoding != "utf8" || !b.includesOrganization(string(cell.Data)) {
				return "", ErrOrganization
			}
		}
		if column.Disposition == "pending" {
			return "", ErrPending
		}
		if column.Disposition == "retired" || column.Disposition == "rebuild" {
			continue
		}
		if _, err := decode(column, row[name]); err != nil {
			return "", err
		}
	}
	return organization, nil
}

func (b *Binding) includesOrganization(organization string) bool {
	if b.source.OrganizationID != "" {
		return organization == b.source.OrganizationID
	}
	_, allowed := b.organizations[organization]
	return allowed
}
