package pages

import (
	"net/http"
	"net/url"
	"server/proxy"

	"github.com/gin-gonic/gin"

	"server/settings"
	"server/torr"
	"server/web/auth"
	"server/web/pages/template"

	"golang.org/x/exp/slices"
)

func SetupRoute(route gin.IRouter) {
	authorized := route.Group("/", auth.CheckAuth())

	webPagesAuth := route.Group("/", func() gin.HandlerFunc {
		return func(c *gin.Context) {
			if slices.Contains([]string{"/site.webmanifest"}, c.FullPath()) {
				return
			}
			auth.CheckAuth()(c)
		}
	}())

	template.RouteWebPages(webPagesAuth)
	authorized.GET("/stat", statPage)
	authorized.GET("/magnets", getTorrents)
	authorized.Any("/proxy/*url", proxyUrl)
}

// stat godoc
//
//	@Summary		TorrServer Statistics
//	@Description	Show server and torrents statistics.
//
//	@Tags			Pages
//
//	@Produce		text/plain
//	@Success		200	"TorrServer statistics"
//	@Router			/stat [get]
func statPage(c *gin.Context) {
	torr.WriteStatus(c.Writer)
	c.Status(200)
}

// getTorrents godoc
//
//	@Summary		Get HTML of magnet links
//	@Description	Get HTML of magnet links.
//
//	@Tags			Pages
//
//	@Produce		text/html
//	@Success		200	"HTML with Magnet links"
//	@Router			/magnets [get]
func getTorrents(c *gin.Context) {
	list := settings.ListTorrent()
	html := "<div>"
	for _, db := range list {
		ts := db.TorrentSpec
		if ts == nil {
			continue
		}
		v := url.Values{}
		v.Set("xt", "urn:btih:"+ts.InfoHash)
		if ts.DisplayName != "" {
			v.Set("dn", ts.DisplayName)
		}
		for _, tier := range ts.Trackers {
			for _, tr := range tier {
				v.Add("tr", tr)
			}
		}
		mag := "magnet:?" + v.Encode()
		html += "<p><a href='" + mag + "'>magnet:?xt=urn:btih:" + ts.InfoHash + "</a></p>"
	}
	html += "</div>"
	c.Data(200, "text/html; charset=utf-8", []byte(html))
}

func proxyUrl(c *gin.Context) {
	if proxy.P2Proxy != nil {
		proxy.P2Proxy.GinHandler(c)
		return
	}
	c.AbortWithStatus(http.StatusNotFound)
}
