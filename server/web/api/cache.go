package api

import (
	"net/http"

	"server/torr"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// Action: get
type cacheReqJS struct {
	requestI
	Hash string `json:"hash,omitempty"`
}

// cache godoc
//
//	@Summary		Return cache stats
//	@Description	Return cache stats.
//
//	@Tags			API
//
//	@Param			request	body	cacheReqJS	true	"Cache stats request"
//
//	@Produce		json
//	@Success		200	{object} state.CacheState	"Cache stats"
//	@Router			/cache [post]
func cache(c *gin.Context) {
	var req cacheReqJS
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}
	c.Status(http.StatusBadRequest)
	switch req.Action {
	case "get":
		{
			getCache(req, c)
		}
	}
}

func getCache(req cacheReqJS, c *gin.Context) {
	if req.Hash == "" {
		c.AbortWithError(http.StatusBadRequest, errors.New("hash is empty"))
		return
	}
	// Read-only lookup: a torrent must stay alive because it is being PLAYED
	// (an active reader), never because its cache map is being polled. The
	// deadline-pushing/promoting GetTorrent let the dialog's 100ms /cache poll
	// keep a torrent resident forever after playback ended; GetTorrentInfo
	// observes without waking it, falling back to the DB record so the dialog
	// still renders a dropped torrent's info (empty cache map) instead of 404.
	tor := torr.GetTorrentInfo(req.Hash)

	if tor != nil {
		st := tor.CacheState()
		if st == nil {
			c.JSON(200, struct{}{})
		} else {
			c.JSON(200, st)
		}
	} else {
		c.Status(http.StatusNotFound)
	}
}
