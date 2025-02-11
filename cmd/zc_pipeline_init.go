package cmd

import (
	"context"
	"fmt"
	"github.com/Azure/azure-pipeline-go/pipeline"

	"github.com/Azure/azure-storage-azcopy/v10/common"
)

func InitPipeline(ctx context.Context, location common.Location, credential common.CredentialInfo, logLevel pipeline.LogLevel, trailingDot common.TrailingDotOption, from common.Location) (p pipeline.Pipeline, err error) {
	switch location {
	case common.ELocation.Local(),
		common.ELocation.Benchmark():
		// Gracefully return
		return nil, nil
	case common.ELocation.BlobFS():
		p, err = createBlobFSPipeline(ctx, credential, logLevel)
	case common.ELocation.File():// Pipelines no longer used for Blob or File since moving to Track 2.
	case common.ELocation.Blob():
	case common.ELocation.S3():
	case common.ELocation.GCP():
		// Gracefully return because pipelines aren't used for S3 or GCP
		return nil, nil
	default:
		err = fmt.Errorf("can't produce new pipeline for location %s", location)
	}

	return
}
