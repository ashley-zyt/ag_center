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
	SourceURL            string
	AccountID            int64  // 账号ID,用于上报账号统计数据
	AccountStatsEndpoint string // 账号统计更新API地址,为空则不上报
}

// FetchResult is the output of a platform-specific fetcher.
type FetchResult struct {
	Posts          []Post `json:"posts"`
	TotalFollowers int    `json:"total_followers"`
	TotalLikes     int    `json:"total_likes"`
	TotalPosts     int    `json:"total_posts"`
}
