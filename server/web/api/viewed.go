package api

import (
	"errors"
	"net/http"

	sets "server/settings"

	"github.com/gin-gonic/gin"
)

var errEmptyHash = errors.New("hash is empty")

/*
file index starts from 1
*/

// Action: set, rem, list
type viewedReqJS struct {
	requestI
	*sets.Viewed
}

// viewed godoc
//
//	@Summary		Set / List / Remove viewed torrents
//	@Description	Allow to set, list or remove viewed torrents from server.
//
//	@Tags			API
//
//	@Param			request	body	viewedReqJS	true	"Viewed torrent request. Available params for action: set, rem, list"
//
//	@Accept			json
//	@Produce		json
//	@Success		200 {array} sets.Viewed
//	@Router			/viewed [post]
func viewed(c *gin.Context) {
	var req viewedReqJS
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	// The embedded *sets.Viewed stays nil when the body has no hash/file_index
	// (a bare {"action":"list"} is valid — it lists everything).
	if req.Viewed == nil {
		req.Viewed = &sets.Viewed{}
	}

	switch req.Action {
	case "set":
		{
			if req.Hash == "" {
				c.AbortWithError(http.StatusBadRequest, errEmptyHash)
				return
			}
			setViewed(req, c)
		}
	case "rem":
		{
			if req.Hash == "" {
				c.AbortWithError(http.StatusBadRequest, errEmptyHash)
				return
			}
			remViewed(req, c)
		}
	case "list":
		{
			listViewed(req, c)
		}
	}
}

func setViewed(req viewedReqJS, c *gin.Context) {
	sets.SetViewed(req.Viewed)
	c.Status(200)
}

func remViewed(req viewedReqJS, c *gin.Context) {
	sets.RemViewed(req.Viewed)
	c.Status(200)
}

func listViewed(req viewedReqJS, c *gin.Context) {
	list := sets.ListViewed(req.Hash)
	if list == nil {
		list = []*sets.Viewed{}
	}
	c.JSON(200, list)
}
