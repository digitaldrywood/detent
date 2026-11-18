package changelanding

import "github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

type Deps struct{ Repository tenantstore.Repository }
type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }
