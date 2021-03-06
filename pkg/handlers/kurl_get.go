package handlers

import (
	"net/http"

	"github.com/replicatedhq/kots/pkg/k8s"
	"github.com/replicatedhq/kots/pkg/kurl"
	"github.com/replicatedhq/kots/pkg/logger"
)

func (h *Handler) GetKurlNodes(w http.ResponseWriter, r *http.Request) {
	client, err := k8s.Clientset()
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	nodes, err := kurl.GetNodes(client)
	if err != nil {
		logger.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	JSON(w, http.StatusOK, nodes)
}
