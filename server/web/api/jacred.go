package api

import (
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"server/jacred"
	"server/rutor/models"
	sets "server/settings"
)

// jacredSearch godoc
//
//	@Summary		Makes a JacRed search
//	@Description	Searches the configured JacRed aggregator.
//
//	@Tags			API
//
//	@Param			query	query	string	true	"JacRed query"
//
//	@Produce		json
//	@Success		200	{array}	models.TorrentDetails	"JacRed torrent search result(s)"
//	@Router			/jacred/search [get]
func jacredSearch(c *gin.Context) {
	if !sets.BTsets().EnableJacRedSearch {
		c.JSON(http.StatusBadRequest, []string{})
		return
	}
	query := c.Query("query")
	query, _ = url.QueryUnescape(query)
	list := jacred.Search(c.Request.Context(), query)
	if list == nil {
		list = []*models.TorrentDetails{}
	}
	c.JSON(200, list)
}

type jacredTestReq struct {
	Host string `json:"host"`
	Key  string `json:"key"`
}

func jacredTest(c *gin.Context) {
	var req jacredTestReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	if err := jacred.Test(c.Request.Context(), req.Host, req.Key); err != nil {
		c.JSON(200, gin.H{"success": false, "error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"success": true})
}
