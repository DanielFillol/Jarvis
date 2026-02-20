// internal/jira/types.go
package jira

// JiraSearchJQLReq is the request payload for the /rest/api/3/search/jql
// endpoint.  The JQL string specifies the search query, and optional
// parameters control paging and the fields returned.
type JiraSearchJQLReq struct {
	JQL        string   `json:"jql"`
	StartAt    int      `json:"startAt,omitempty"`
	MaxResults int      `json:"maxResults,omitempty"`
	Fields     []string `json:"fields,omitempty"`
}

// JiraSearchJQLResp models the response from the /rest/api/3/search/jql
// endpoint.  Only a subset of fields is defined.
type JiraSearchJQLResp struct {
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
		} `json:"fields"`
	} `json:"issues"`
}

// JiraSearchJQLRespIssue is an internal flattened representation of a
// Jira issue used by higher-level code to build context for the LLM.
type JiraSearchJQLRespIssue struct {
	Key      string
	Project  string
	Type     string
	Status   string
	Priority string
	Assignee string
	Summary  string
	Updated  string
}

// JiraIssueResp models the response from Jira's GET issue endpoint.  It
// contains both rendered and raw fields.  Only fields accessed in
//
//	the current code are defined here.
type JiraIssueResp struct {
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

// JiraCreateIssueResp represents the response from Jira's creation issue
// API.  Only a subset of fields is defined here.
type JiraCreateIssueResp struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Self string `json:"self"`
}
