// Package scraper defines the shared types used when scraping the first
// page of posts from a social-media account. The platform-specific fetch
// functions live in their own packages (twitter, youtube, instagram,
// tiktok) and the dispatcher is composed in cmd/server.
package scraper

// Post is a single post entry scraped from a platform's account page.
type Post struct {
	Title       string `json:"title"`
	Link        string `json:"link"`
	PublishTime string `json:"publish_time"`
	Likes       int    `json:"likes"`
	Comments    int    `json:"comments"`
	Shares      int    `json:"shares"`
	Views       int    `json:"views"`
}

// FetchRequest is the input passed to a platform-specific fetcher.
type FetchRequest struct {
	WebsocketURL string
	SourceURL    string
}

// FetchResult is the output of a platform-specific fetcher.
type FetchResult struct {
	Posts []Post `json:"posts"`
}
