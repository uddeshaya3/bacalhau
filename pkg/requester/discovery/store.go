package discovery

import (
	"context"

	"github.com/filecoin-project/bacalhau/pkg/model"
	"github.com/filecoin-project/bacalhau/pkg/requester"
)

type StoreNodeDiscovererParams struct {
	Store requester.NodeInfoStore
}

type StoreNodeDiscoverer struct {
	store requester.NodeInfoStore
}

func NewStoreNodeDiscoverer(params StoreNodeDiscovererParams) *StoreNodeDiscoverer {
	return &StoreNodeDiscoverer{
		store: params.Store,
	}
}

// FindNodes returns the nodes that support the job's execution engine, and have enough TOTAL capacity to run the job.
func (d *StoreNodeDiscoverer) FindNodes(ctx context.Context, job model.Job) ([]model.NodeInfo, error) {
	// filter nodes that support the job's engine
	return d.store.ListForEngine(ctx, job.Spec.Engine)
}

// compile time check that StoreNodeDiscoverer implements NodeDiscoverer
var _ requester.NodeDiscoverer = (*StoreNodeDiscoverer)(nil)
