package api

import (
	"net/http"

	"server/rutor"

	"server/bonjour"
	"server/dlna"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"

	sets "server/settings"
	"server/torr"
)

// Action: get, set, def
type setsReqJS struct {
	requestI
	Sets *sets.BTSets `json:"sets,omitempty"`
}

// settings godoc
//
//	@Summary		Get / Set server settings
//	@Description	Allow to get or set server settings.
//
//	@Tags			API
//
//	@Param			request	body	setsReqJS	true	"Settings request. Available params for action: get, set, def"
//
//	@Accept			json
//	@Produce		json
//	@Success		200	{object}	sets.BTSets	"Settings JSON or nothing. Depends on what action has been asked."
//	@Router			/settings [post]
func settings(c *gin.Context) {
	var req setsReqJS
	err := c.ShouldBindJSON(&req)
	if err != nil {
		abortWithJSONError(c, http.StatusBadRequest, err)
		return
	}

	if req.Action == "get" {
		c.JSON(200, sets.BTsets())
		return
	} else if req.Action == "set" {
		if req.Sets == nil {
			abortWithJSONError(c, http.StatusBadRequest, errors.New("sets is empty"))
			return
		}
		torr.SetSettings(req.Sets)
		restartServices()
		c.Status(200)
		return
	} else if req.Action == "def" {
		torr.SetDefSettings()
		// The defaults differ from what was running (Bonjour is on by default),
		// so the services follow the settings now in effect instead of just
		// being stopped until the next restart.
		restartServices()
		c.Status(200)
		return
	}
	abortWithJSONError(c, http.StatusBadRequest, errors.New("action is empty"))
}

// restartServices applies the settings now in effect to DLNA, Bonjour and the
// rutor search.
func restartServices() {
	dlna.Stop()
	if sets.BTsets() != nil && sets.BTsets().EnableDLNA {
		dlna.Start()
	}
	bonjour.Stop()
	if sets.BTsets() != nil && sets.BTsets().EnableBonjour {
		bonjour.Start()
	}
	rutor.Stop()
	rutor.Start()
}
