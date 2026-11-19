package model

type Info struct {
	Path              string
	Key               string
	Branch            string
	ReviewBranch      string
	PublicationBranch string
	Created           bool
}

func (i Info) PublishBranch() string {
	if i.PublicationBranch != "" {
		return i.PublicationBranch
	}
	return i.Branch
}
