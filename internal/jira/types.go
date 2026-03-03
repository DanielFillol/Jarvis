package jira

// SearchJQLReq is the request payload for the /rest/api/3/search/jql
// endpoint.  The JQL string specifies the search query, and optional
// parameters control paging and the fields returned.
type SearchJQLReq struct {
	JQL        string   `json:"jql"`
	StartAt    int      `json:"startAt,omitempty"`
	MaxResults int      `json:"maxResults,omitempty"`
	Fields     []string `json:"fields,omitempty"`
}

// SearchJQLResp models the response from the /rest/api/3/search/jql
// endpoint.  Only a subset of fields is defined.
type SearchJQLResp struct {
	Issues []struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			Updated string `json:"updated"`
			Status  struct {
				Name string `json:"name"`
			} `json:"status"`
			IssueType struct {
				Name string `json:"name"`
			} `json:"issuetype"`
			Priority struct {
				Name string `json:"name"`
			} `json:"priority"`
			Assignee *struct {
				DisplayName string `json:"displayName"`
			} `json:"assignee"`
			Project struct {
				Key string `json:"key"`
			} `json:"project"`
			// Custom-field_10020 is the standard Jira Cloud sprint field.
			// It is an array; the last active (or most recent) entry is used.
			Sprint []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"customfield_10020"`
		} `json:"fields"`
	} `json:"issues"`
}

// SearchJQLRespIssue is an internal flattened representation of a
// Jira issue used by higher-level code to build context for the LLM.
type SearchJQLRespIssue struct {
	Key      string
	Project  string
	Type     string
	Status   string
	Priority string
	Assignee string
	Summary  string
	Updated  string
	Sprint   string
}

// IssueResp models the response from Jira's GET issue endpoint.  It
// contains both rendered and raw fields.  Only fields accessed in
//
//	the current code are defined here.
type IssueResp struct {
	Key            string `json:"key"`
	RenderedFields struct {
		Description string `json:"description"`
	} `json:"renderedFields"`
	Fields struct {
		Summary string `json:"summary"`
		Status  struct {
			Name string `json:"name"`
		} `json:"status"`
		IssueType struct {
			Name string `json:"name"`
		} `json:"issuetype"`
		Priority struct {
			Name string `json:"name"`
		} `json:"priority"`
		Assignee *struct {
			DisplayName string `json:"displayName"`
		} `json:"assignee"`
		Description any `json:"description"`
		Parent      *struct {
			Key    string `json:"key"`
			Fields struct {
				Summary string `json:"summary"`
			} `json:"fields"`
		} `json:"parent"`
		Subtasks []struct {
			Key    string `json:"key"`
			Fields struct {
				Summary string `json:"summary"`
				Status  struct {
					Name string `json:"name"`
				} `json:"status"`
				IssueType struct {
					Name string `json:"name"`
				} `json:"issuetype"`
			} `json:"fields"`
		} `json:"subtasks"`
	} `json:"fields"`
}

// IssueDraft represents a draft of a Jira issue used when the user
// requests creation of a new card.  Optional fields may be left blank
// and later filled in by the user.
type IssueDraft struct {
	Project     string   `json:"project"`
	IssueType   string   `json:"issue_type"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Priority    string   `json:"priority"`
	Labels      []string `json:"labels"`
}

// CreateIssueResp represents the response from Jira's creation issue
// API.  Only a subset of fields is defined here.
type CreateIssueResp struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Self string `json:"self"`
}

// ProjectInfo holds a minimal project summary returned by ListProjects.
type ProjectInfo struct {
	Key  string
	Name string
}
