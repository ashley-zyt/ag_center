package models

type FetchPostDataRequest struct {
	ID               int           `json:"id"`
	ProfileName      string        `json:"profile_name"`
	ActiveAccounts   []AccountInfo `json:"active_accounts"`
	Host             string        `json:"host"`
	Port             int           `json:"port"`
	WaitSeconds      int           `json:"wait_seconds"`
	UndetectablePath string        `json:"undetectable_path"`
}

type AccountInfo struct {
	ID        int    `json:"id"`
	Platform  string `json:"platform"`
	SourceURL string `json:"source_url"`
}

type PostData struct {
	Title       string `json:"title"`
	Link        string `json:"link"`
	PublishTime string `json:"publish_time"`
	Likes       int    `json:"likes"`
	Comments    int    `json:"comments"`
	Shares      int    `json:"shares"`
	Views       int    `json:"views"`
}

type AccountPostData struct {
	AccountID int        `json:"account_id"`
	Platform  string     `json:"platform"`
	Posts     []PostData `json:"posts"`
	ErrorInfo string     `json:"error_info,omitempty"`
}

type FetchPostDataResponse struct {
	Type    string            `json:"type"`
	Results []AccountPostData `json:"results"`
}
