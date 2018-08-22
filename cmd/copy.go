// Copyright © 2017 Microsoft <wastore@microsoft.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cmd

import (
			"context"
	"encoding/json"
	"errors"
	"fmt"

	"io"
	"net/url"
	"os"
	"strings"
		"time"

	"github.com/Azure/azure-storage-azcopy/common"
	"github.com/Azure/azure-storage-azcopy/ste"
	"github.com/Azure/azure-storage-blob-go/2018-03-28/azblob"
	"github.com/Azure/azure-storage-file-go/2017-07-29/azfile"
	"github.com/spf13/cobra"
)

// upload related
const uploadMaxTries = 5
const uploadTryTimeout = time.Minute * 10
const uploadRetryDelay = time.Second * 1
const uploadMaxRetryDelay = time.Second * 3

// download related
const downloadMaxTries = 5
const downloadTryTimeout = time.Minute * 10
const downloadRetryDelay = time.Second * 1
const downloadMaxRetryDelay = time.Second * 3

const pipingUploadParallelism = 5
const pipingDefaultBlockSize = 8 * 1024 * 1024

const pipeLocation = "~pipe~"

// represents the raw copy command input from the user
type rawCopyCmdArgs struct {
	// from arguments
	src    string
	dst    string
	fromTo string
	//blobUrlForRedirection string

	// filters from flags
	include        string
	exclude        string
	recursive      bool
	followSymlinks bool
	withSnapshots  bool
	// forceWrite flag is used to define the User behavior
	// to overwrite the existing blobs or not.
	forceWrite bool

	// options from flags
	blockSize                uint32
	metadata                 string
	contentType              string
	contentEncoding          string
	noGuessMimeType          bool
	preserveLastModifiedTime bool
	blockBlobTier            string
	pageBlobTier             string
	background               bool
	output                   string
	acl                      string
	logVerbosity             string
	cancelFromStdin          bool
	// oauth options
	useInteractiveOAuthUserCredential bool
	tenantID                          string
	aadEndpoint                       string
}

// validates and transform raw input into cooked input
func (raw rawCopyCmdArgs) cook() (cookedCopyCmdArgs, error) {
	cooked := cookedCopyCmdArgs{}

	fromTo, err := validateFromTo(raw.src, raw.dst, raw.fromTo) // TODO: src/dst
	if err != nil {
		return cooked, err
	}
	cooked.source = raw.src
	cooked.destination = raw.dst

	cooked.fromTo = fromTo

	// copy&transform flags to type-safety
	cooked.recursive = raw.recursive
	cooked.followSymlinks = raw.followSymlinks
	cooked.withSnapshots = raw.withSnapshots
	cooked.forceWrite = raw.forceWrite
	cooked.blockSize = raw.blockSize

	err = cooked.blockBlobTier.Parse(raw.blockBlobTier)
	if err != nil {
		return cooked, err
	}
	err = cooked.pageBlobTier.Parse(raw.pageBlobTier)
	if err != nil {
		return cooked, err
	}
	err = cooked.logVerbosity.Parse(raw.logVerbosity)
	if err != nil {
		return cooked, err
	}

	// initialize the include map which contains the list of files to be included
	// parse the string passed in include flag
	// more than one file are expected to be separated by ';'
	cooked.include = make(map[string]int)
	if len(raw.include) > 0 {
		files := strings.Split(raw.include, ";")
		for index := range files {
			// If split of the include string leads to an empty string
			// not include that string
			if len(files[index]) == 0 {
				continue
			}
			cooked.include[files[index]] = index
		}
	}

	// initialize the exclude map which contains the list of files to be excluded
	// parse the string passed in exclude flag
	// more than one file are expected to be separated by ';'
	cooked.exclude = make(map[string]int)
	if len(raw.exclude) > 0 {
		files := strings.Split(raw.exclude, ";")
		for index := range files {
			// If split of the include string leads to an empty string
			// not include that string
			if len(files[index]) == 0 {
				continue
			}
			cooked.exclude[files[index]] = index
		}
	}

	cooked.metadata = raw.metadata
	cooked.contentType = raw.contentType
	cooked.contentEncoding = raw.contentEncoding
	cooked.noGuessMimeType = raw.noGuessMimeType
	cooked.preserveLastModifiedTime = raw.preserveLastModifiedTime
	cooked.background = raw.background
	cooked.output.Parse(raw.output)
	cooked.acl = raw.acl
	cooked.cancelFromStdin = raw.cancelFromStdin

	// cook oauth parameters
	cooked.useInteractiveOAuthUserCredential = raw.useInteractiveOAuthUserCredential
	cooked.tenantID = raw.tenantID
	cooked.aadEndpoint = raw.aadEndpoint
	// generate a unique job ID
	cooked.jobID = common.NewJobID()

	// check for the flag value relative to fromTo location type
	// Example1: for Local to Blob, preserve-last-modified-time flag should not be set to true
	// Example2: for Blob to Local, follow-symlinks, blob-tier flags should not be provided with values.
	switch cooked.fromTo {
	case common.EFromTo.LocalBlob():
		if cooked.preserveLastModifiedTime {
			return cooked, fmt.Errorf("preserve-last-modified-time is set to true while uploading")
		}
	case common.EFromTo.LocalFile():
		if cooked.preserveLastModifiedTime {
			return cooked, fmt.Errorf("preserve-last-modified-time is set to true while uploading")
		}
		if cooked.blockBlobTier != common.EBlockBlobTier.None() ||
			cooked.pageBlobTier != common.EPageBlobTier.None() {
			return cooked, fmt.Errorf("blob-tier is set while downloading")
		}
	case common.EFromTo.BlobLocal(),
		common.EFromTo.FileLocal():
		if cooked.followSymlinks {
			return cooked, fmt.Errorf("follow-symlinks flag is set to true while downloading")
		}
		if cooked.blockBlobTier != common.EBlockBlobTier.None() ||
			cooked.pageBlobTier != common.EPageBlobTier.None() {
			return cooked, fmt.Errorf("blob-tier is set while downloading")
		}
		if cooked.noGuessMimeType {
			return cooked, fmt.Errorf("no-guess-mime-type is set while downloading")
		}
		if len(cooked.contentType) > 0 || len(cooked.contentEncoding) > 0 || len(cooked.metadata) > 0 {
			return cooked, fmt.Errorf("content-type, content-encoding or metadata is set while downloading")
		}
	case common.EFromTo.BlobBlob(),
		common.EFromTo.FileBlob():
		if cooked.preserveLastModifiedTime {
			return cooked, fmt.Errorf("preserve-last-modified-time is set to true while copying from sevice to service")
		}
		if cooked.followSymlinks {
			return cooked, fmt.Errorf("follow-symlinks flag is set to true while copying from sevice to service")
		}
		if cooked.blockBlobTier != common.EBlockBlobTier.None() ||
			cooked.pageBlobTier != common.EPageBlobTier.None() {
			return cooked, fmt.Errorf("blob-tier is set while copying from sevice to service")
		}
		if cooked.noGuessMimeType {
			return cooked, fmt.Errorf("no-guess-mime-type is set while copying from sevice to service")
		}
		if len(cooked.contentType) > 0 || len(cooked.contentEncoding) > 0 || len(cooked.metadata) > 0 {
			return cooked, fmt.Errorf("content-type, content-encoding or metadata is set while copying from sevice to service")
		}
	}
	return cooked, nil
}

// represents the processed copy command input from the user
type cookedCopyCmdArgs struct {
	// from arguments
	source         string
	sourceSAS      string
	destination    string
	destinationSAS string
	fromTo         common.FromTo

	// filters from flags
	include        map[string]int
	exclude        map[string]int
	recursive      bool
	followSymlinks bool
	withSnapshots  bool
	forceWrite     bool

	// options from flags
	blockSize                uint32
	blockBlobTier            common.BlockBlobTier
	pageBlobTier             common.PageBlobTier
	metadata                 string
	contentType              string
	contentEncoding          string
	noGuessMimeType          bool
	preserveLastModifiedTime bool
	background               bool
	output                   common.OutputFormat
	acl                      string
	logVerbosity             common.LogLevel
	cancelFromStdin          bool
	// oauth options
	useInteractiveOAuthUserCredential bool
	tenantID                          string
	aadEndpoint                       string
	// commandString hold the user given command which is logged to the Job log file
	commandString string

	// generated
	jobID common.JobID

	// variables used to calculate progress
	// intervalStartTime holds the last time value when the progress summary was fetched
	// the value of this variable is used to calculate the throughput
	// it gets updated every time the progress summary is fetched
	intervalStartTime        time.Time
	intervalBytesTransferred uint64

	// used to calculate job summary
	jobStartTime time.Time

	// this flag is set by the enumerator
	// it is useful to indicate whether we are simply waiting for the purpose of cancelling
	isEnumerationComplete bool
}

func (cca *cookedCopyCmdArgs) isRedirection() bool {
	switch cca.fromTo {
	// File's piping is not supported temporarily.
	// case common.EFromTo.PipeFile():
	// 	fallthrough
	// case common.EFromTo.FilePipe():
	// 	fallthrough
	case common.EFromTo.BlobPipe():
		fallthrough
	case common.EFromTo.PipeBlob():
		return true
	default:
		return false
	}
}

func (cca *cookedCopyCmdArgs) process() error {
	if cca.isRedirection() {
		err := cca.processRedirectionCopy()

		if err != nil {
			return err
		}

		// if no error, the operation is now complete
		glcm.Exit("", common.EExitCode.Success())
	}
	return cca.processCopyJobPartOrders()
}

// TODO discuss with Jeff what features should be supported by redirection, such as metadata, content-type, etc.
func (cca *cookedCopyCmdArgs) processRedirectionCopy() error {
	if cca.fromTo == common.EFromTo.PipeBlob() {
		return cca.processRedirectionUpload(cca.destination, cca.blockSize)
	} else if cca.fromTo == common.EFromTo.BlobPipe() {
		return cca.processRedirectionDownload(cca.source)
	}

	return fmt.Errorf("unsupported redirection type: %s", cca.fromTo)
}

func (cca *cookedCopyCmdArgs) processRedirectionDownload(blobUrl string) error {
	// step 0: check the Stdout before uploading
	_, err := os.Stdout.Stat()
	if err != nil {
		return fmt.Errorf("fatal: cannot write to Stdout due to error: %s", err.Error())
	}

	// step 1: initialize pipeline
	p := azblob.NewPipeline(azblob.NewAnonymousCredential(), azblob.PipelineOptions{
		Retry: azblob.RetryOptions{
			Policy:        azblob.RetryPolicyExponential,
			MaxTries:      downloadMaxTries,
			TryTimeout:    downloadTryTimeout,
			RetryDelay:    downloadRetryDelay,
			MaxRetryDelay: downloadMaxRetryDelay,
		},
		Telemetry: azblob.TelemetryOptions{
			Value: common.UserAgent,
		},
	})

	// step 2: parse source url
	u, err := url.Parse(blobUrl)
	if err != nil {
		return fmt.Errorf("fatal: cannot parse source blob URL due to error: %s", err.Error())
	}

	// step 3: start download
	blobURL := azblob.NewBlobURL(*u, p)
	blobStream, err := blobURL.Download(context.TODO(), 0, azblob.CountToEnd, azblob.BlobAccessConditions{}, false)
	if err != nil {
		return fmt.Errorf("fatal: cannot download blob due to error: %s", err.Error())
	}

	blobBody := blobStream.Body(azblob.RetryReaderOptions{MaxRetryRequests: downloadMaxTries})
	defer blobBody.Close()

	// step 4: pipe everything into Stdout
	_, err = io.Copy(os.Stdout, blobBody)
	if err != nil {
		return fmt.Errorf("fatal: cannot download blob to Stdout due to error: %s", err.Error())
	}

	return nil
}

func (cca *cookedCopyCmdArgs) processRedirectionUpload(blobUrl string, blockSize uint32) error {
	// if no block size is set, then use default value
	if blockSize == 0 {
		blockSize = pipingDefaultBlockSize
	}

	// step 0: initialize pipeline
	p := azblob.NewPipeline(azblob.NewAnonymousCredential(), azblob.PipelineOptions{
		Retry: azblob.RetryOptions{
			Policy:        azblob.RetryPolicyExponential,
			MaxTries:      uploadMaxTries,
			TryTimeout:    uploadTryTimeout,
			RetryDelay:    uploadRetryDelay,
			MaxRetryDelay: uploadMaxRetryDelay,
		},
		Telemetry: azblob.TelemetryOptions{
			Value: common.UserAgent,
		},
	})

	// step 1: parse destination url
	u, err := url.Parse(blobUrl)
	if err != nil {
		return fmt.Errorf("fatal: cannot parse destination blob URL due to error: %s", err.Error())
	}

	// step 2: leverage high-level call in Blob SDK to upload stdin in parallel
	blockBlobUrl := azblob.NewBlockBlobURL(*u, p)
	_, err = azblob.UploadStreamToBlockBlob(context.TODO(), os.Stdin, blockBlobUrl, azblob.UploadStreamToBlockBlobOptions{
		//BufferSize: pipingDefaultBlockSize,
		BufferSize: pipingDefaultBlockSize,
		MaxBuffers: pipingUploadParallelism,
	})

	return err
}

// validateCredentialType validate if given credential type is supported with specific copy scenario
func (cca cookedCopyCmdArgs) validateCredentialType(credentialType common.CredentialType) error {
	// oAuthToken is only supported by Blob/BlobFS.
	if credentialType == common.ECredentialType.OAuthToken() &&
		!(cca.fromTo == common.EFromTo.LocalBlob() || cca.fromTo == common.EFromTo.BlobLocal() ||
			cca.fromTo == common.EFromTo.LocalBlobFS() || cca.fromTo == common.EFromTo.BlobFSLocal()) {
		return fmt.Errorf("OAuthToken is not supported for FromTo: %v", cca.fromTo)
	}

	return nil
}

// getCredentialType checks user provided commandline switches, and gets the proper credential type
// for current copy command.
func (cca cookedCopyCmdArgs) getCredentialType() (credentialType common.CredentialType, err error) {
	credentialType = common.ECredentialType.Unknown()

	if cca.useInteractiveOAuthUserCredential { // User explicty specify to use interactive login per command-line
		credentialType = common.ECredentialType.OAuthToken()
	} else {
		// Could be using oauth session mode or non-oauth scenario which uses SAS authentication or public endpoint,
		// verify credential type with cached token info, src or dest blob resource URL.
		switch cca.fromTo {
		case common.EFromTo.BlobBlob():
			// For blob to blob copy, calculate credential type for destination (currently only support StageBlockFromURL)
			// If the traditional approach(download+upload) need be supported, credential type should be calculated for both src and dest.
			fallthrough
		case common.EFromTo.LocalBlob():
			credentialType, err = getBlobCredentialType(context.Background(), cca.destination, false)
			if err != nil {
				return common.ECredentialType.Unknown(), err
			}
		case common.EFromTo.BlobLocal():
			credentialType, err = getBlobCredentialType(context.Background(), cca.source, true)
			if err != nil {
				return common.ECredentialType.Unknown(), err
			}
		case common.EFromTo.LocalBlobFS():
			fallthrough
		case common.EFromTo.BlobFSLocal():
			credentialType, err = getBlobFSCredentialType()
			if err != nil {
				return common.ECredentialType.Unknown(), err
			}
		default:
			credentialType = common.ECredentialType.Anonymous()
			//glcm.Info(fmt.Sprintf("Use anonymous credential by default for FromTo '%v'", cca.fromTo))
		}
	}

	if cca.validateCredentialType(credentialType) != err {
		return common.ECredentialType.Unknown(), err
	}

	return credentialType, nil
}

// handles the copy command
// dispatches the job order (in parts) to the storage engine
func (cca *cookedCopyCmdArgs) processCopyJobPartOrders() (err error) {
	// initialize the fields that are constant across all job part orders
	jobPartOrder := common.CopyJobPartOrderRequest{
		JobID:      cca.jobID,
		FromTo:     cca.fromTo,
		ForceWrite: cca.forceWrite,
		Priority:   common.EJobPriority.Normal(),
		LogLevel:   cca.logVerbosity,
		Include:    cca.include,
		Exclude:    cca.exclude,
		BlobAttributes: common.BlobTransferAttributes{
			BlockSizeInBytes:         cca.blockSize,
			ContentType:              cca.contentType,
			ContentEncoding:          cca.contentEncoding,
			BlockBlobTier:            cca.blockBlobTier,
			PageBlobTier:             cca.pageBlobTier,
			Metadata:                 cca.metadata,
			NoGuessMimeType:          cca.noGuessMimeType,
			PreserveLastModifiedTime: cca.preserveLastModifiedTime,
		},
		// source sas is stripped from the source given by the user and it will not be stored in the part plan file.
		SourceSAS: cca.sourceSAS,

		// destination sas is stripped from the destination given by the user and it will not be stored in the part plan file.
		DestinationSAS: cca.destinationSAS,
		CommandString:  cca.commandString,
		CredentialInfo: common.CredentialInfo{},
	}

	// verifies credential type and initializes credential info.
	jobPartOrder.CredentialInfo.CredentialType, err = cca.getCredentialType()
	if err != nil {
		return err
	}
	//glcm.Info(fmt.Sprintf("Copy uses credential type %q.", jobPartOrder.CredentialInfo.CredentialType))
	// For OAuthToken credential, assign OAuthTokenInfo to CopyJobPartOrderRequest properly,
	// the info will be transferred to STE.
	if jobPartOrder.CredentialInfo.CredentialType == common.ECredentialType.OAuthToken() {
		uotm := GetUserOAuthTokenManagerInstance()

		var tokenInfo *common.OAuthTokenInfo
		if cca.useInteractiveOAuthUserCredential { // Scenario-1: interactive login per copy command
			tokenInfo, err = uotm.LoginWithADEndpoint(cca.tenantID, cca.aadEndpoint, false)
			if err != nil {
				return err
			}
		} else if tokenInfo, err = uotm.GetTokenInfoFromEnvVar(); err == nil || !common.IsErrorEnvVarOAuthTokenInfoNotSet(err) {
			// Scenario-Test: unattended testing with oauthTokenInfo set through environment variable
			// Note: Scenario-Test has higher priority than scenario-2, so whenever environment variable is set in the context,
			// it will overwrite the cached token info.
			if err != nil { // this is the case when env var exists while get token info failed
				return err
			}
		} else { // Scenario-2: session mode which get token from cache
			tokenInfo, err = uotm.GetCachedTokenInfo()
			if err != nil {
				return err
			}
		}
		jobPartOrder.CredentialInfo.OAuthTokenInfo = *tokenInfo
	}

	from := cca.fromTo.From()
	to := cca.fromTo.To()
	// If the Credentials is of Anonymous Type i.e SAS, then we need to strip the SAS from the credentials
	if jobPartOrder.CredentialInfo.CredentialType == common.ECredentialType.Anonymous() {
		switch from {
		case common.ELocation.Blob():
			fromUrl, err := url.Parse(cca.source)
			if err != nil {
				return fmt.Errorf("error parsing the source url %s. Failed with error %s", fromUrl.String(), err.Error())
			}
			blobParts := azblob.NewBlobURLParts(*fromUrl)
			cca.sourceSAS = blobParts.SAS.Encode()
			jobPartOrder.SourceSAS = cca.sourceSAS
			blobParts.SAS = azblob.SASQueryParameters{}
			bUrl := blobParts.URL()
			cca.source = bUrl.String()
		case common.ELocation.File():
			fromUrl, err := url.Parse(cca.source)
			if err != nil {
				return fmt.Errorf("error parsing the source url %s. Failed with error %s", fromUrl.String(), err.Error())
			}
			fileParts := azfile.NewFileURLParts(*fromUrl)
			cca.sourceSAS = fileParts.SAS.Encode()
			jobPartOrder.SourceSAS = cca.sourceSAS
			fileParts.SAS = azfile.SASQueryParameters{}
			fUrl := fileParts.URL()
			cca.source = fUrl.String()
		}

		switch to {
		case common.ELocation.Blob():
			toUrl, err := url.Parse(cca.destination)
			if err != nil {
				return fmt.Errorf("error parsing the source url %s. Failed with error %s", toUrl.String(), err.Error())
			}
			blobParts := azblob.NewBlobURLParts(*toUrl)
			cca.destinationSAS = blobParts.SAS.Encode()
			jobPartOrder.DestinationSAS = cca.destinationSAS
			blobParts.SAS = azblob.SASQueryParameters{}
			bUrl := blobParts.URL()
			cca.destination = bUrl.String()
		case common.ELocation.File():
			toUrl, err := url.Parse(cca.destination)
			if err != nil {
				return fmt.Errorf("error parsing the source url %s. Failed with error %s", toUrl.String(), err.Error())
			}
			fileParts := azfile.NewFileURLParts(*toUrl)
			cca.destinationSAS = fileParts.SAS.Encode()
			jobPartOrder.DestinationSAS = cca.destinationSAS
			fileParts.SAS = azfile.SASQueryParameters{}
			fUrl := fileParts.URL()
			cca.destination = fUrl.String()
		}
	}

	if from == common.ELocation.Local() {
		// If the path separator is '\\', it means
		// local path is a windows path
		// To avoid path separator check and handling the windows
		// path differently, replace the path separator with the
		// the linux path separator '/'
		if os.PathSeparator == '\\' {
			cca.source = strings.Replace(cca.source, common.OS_PATH_SEPARATOR, "/", -1)
		}
	}

	if to == common.ELocation.Local() {
		// If the path separator is '\\', it means
		// local path is a windows path
		// To avoid path separator check and handling the windows
		// path differently, replace the path separator with the
		// the linux path separator '/'
		if os.PathSeparator == '\\' {
			cca.destination = strings.Replace(cca.destination, common.OS_PATH_SEPARATOR, "/", -1)
		}
	}

	// lastPartNumber determines the last part number order send for the Job.
	var lastPartNumber common.PartNumber
	// depending on the source and destination type, we process the cp command differently
	// Create enumerator and do enumerating
	switch cca.fromTo {
	case common.EFromTo.LocalBlob():
		fallthrough
	case common.EFromTo.LocalBlobFS():
		fallthrough
	case common.EFromTo.LocalFile():
		e := copyUploadEnumerator(jobPartOrder)
		err = e.enumerate(cca)
		lastPartNumber = e.PartNum
	case common.EFromTo.BlobLocal():
		e := copyDownloadBlobEnumerator(jobPartOrder)
		err = e.enumerate(cca)
		lastPartNumber = e.PartNum
	case common.EFromTo.FileLocal():
		e := copyDownloadFileEnumerator(jobPartOrder)
		err = e.enumerate(cca)
		lastPartNumber = e.PartNum
	case common.EFromTo.BlobFSLocal():
		e := copyDownloadBlobFSEnumerator(jobPartOrder)
		err = e.enumerate(cca)
		lastPartNumber = e.PartNum
	case common.EFromTo.BlobTrash():
		e := removeBlobEnumerator(jobPartOrder)
		err = e.enumerate(cca)
		lastPartNumber = e.PartNum
	case common.EFromTo.FileTrash():
		e := removeFileEnumerator(jobPartOrder)
		err = e.enumerate(cca)
		lastPartNumber = e.PartNum
	case common.EFromTo.BlobBlob():
		e := copyBlobToNEnumerator{
			copyS2SEnumerator: copyS2SEnumerator{
				CopyJobPartOrderRequest: jobPartOrder,
			},
		}
		err = e.enumerate(cca)
		lastPartNumber = e.PartNum
	// TODO: Hide the File to Blob direction temporarily, as service support on-going.
	// case common.EFromTo.FileBlob():
	// 	e := copyFileToNEnumerator(jobPartOrder)
	// 	err = e.enumerate(cca)
	// 	lastPartNumber = e.PartNum
	default:
		return fmt.Errorf("copy direction %v is not supported\n", cca.fromTo)
	}

	if err != nil {
		return fmt.Errorf("cannot start job due to error: %s.\n", err)
	}

	// in background mode we would spit out the job id and quit
	// in foreground mode we would continuously print out status updates for the job, so the job id is not important
	if cca.background {
		return nil
	}

	// If there is only one part, then start fetching the JobPart Order.
	if lastPartNumber == 0 {
		cca.waitUntilJobCompletion(false)
	}
	return nil
}

// wraps call to lifecycle manager to wait for the job to complete
// if blocking is specified to true, then this method will never return
// if blocking is specified to false, then another goroutine spawns and wait out the job
func (cca *cookedCopyCmdArgs) waitUntilJobCompletion(blocking bool) {
	// print initial message to indicate that the job is starting
	glcm.Info("\nJob " + cca.jobID.String() + " has started\n")
	currentDir, _ := os.Getwd()
	glcm.Info(fmt.Sprintf("%s.log file created in %s", cca.jobID, currentDir))

	// initialize the times necessary to track progress
	cca.jobStartTime = time.Now()
	cca.intervalStartTime = time.Now()
	cca.intervalBytesTransferred = 0

	// hand over control to the lifecycle manager if blocking
	if blocking {
		glcm.InitiateProgressReporting(cca, !cca.cancelFromStdin)
		glcm.SurrenderControl()
	} else {
		// non-blocking, return after spawning a go routine to watch the job
		glcm.InitiateProgressReporting(cca, !cca.cancelFromStdin)
	}
}

func (cca *cookedCopyCmdArgs) Cancel(lcm common.LifecycleMgr) {
	// prompt for confirmation, except when:
	// 1. output is in json format
	// 2. azcopy was spawned by another process (cancelFromStdin indicates this)
	// 3. enumeration is complete
	if !(cca.output == common.EOutputFormat.Json() || cca.cancelFromStdin || cca.isEnumerationComplete) {
		answer := lcm.Prompt("The source enumeration is not complete, cancelling the job at this point means it cannot be resumed. Please confirm with y/n: ")

		// read a line from stdin, if the answer is not yes, then abort cancel by returning
		if !strings.EqualFold(answer, "y") {
			return
		}
	}

	err := cookedCancelCmdArgs{jobID: cca.jobID}.process()
	if err != nil {
		lcm.Exit("error occurred while cancelling the job "+cca.jobID.String()+". Failed with error "+err.Error(), common.EExitCode.Error())
	}
}

func (cca *cookedCopyCmdArgs) ReportProgressOrExit(lcm common.LifecycleMgr) {
	// fetch a job status
	var summary common.ListJobSummaryResponse
	Rpc(common.ERpcCmd.ListJobSummary(), &cca.jobID, &summary)
	jobDone := summary.JobStatus == common.EJobStatus.Completed() || summary.JobStatus == common.EJobStatus.Cancelled()

	// if json output is desired, simply marshal and return
	// note that if job is already done, we simply exit
	if cca.output == common.EOutputFormat.Json() {
		jsonOutput, err := json.MarshalIndent(summary, "", "  ")
		common.PanicIfErr(err)

		if jobDone {
			exitCode := common.EExitCode.Success()
			if summary.TransfersFailed > 0 {
				exitCode = common.EExitCode.Error()
			}
			lcm.Exit(string(jsonOutput), exitCode)
		} else {
			lcm.Info(string(jsonOutput))
			return
		}
	}

	// if json is not desired, and job is done, then we generate a special end message to conclude the job
	if jobDone {
		duration := time.Now().Sub(cca.jobStartTime) // report the total run time of the job
		exitCode := common.EExitCode.Success()
		if summary.TransfersFailed > 0 {
			exitCode = common.EExitCode.Error()
		}
		lcm.Exit(fmt.Sprintf(
			"\n\nJob %s summary\nElapsed Time (Minutes): %v\nTotal Number Of Transfers: %v\nNumber of Transfers Completed: %v\nNumber of Transfers Failed: %v\n Number of Transfers Skipped: %v\n Final Job Status: %v\n TotalBytesTransferred: %v\n",
			summary.JobID.String(),
			ste.ToFixed(duration.Minutes(), 4),
			summary.TotalTransfers,
			summary.TransfersCompleted,
			summary.TransfersFailed,
			summary.TransfersSkipped,
			summary.JobStatus, summary.TotalBytesTransferred), exitCode)
	}

	// if json is not needed, and job is not done, then we generate a message that goes nicely on the same line
	// display a scanning keyword if the job is not completely ordered
	var scanningString = ""
	if !summary.CompleteJobOrdered {
		scanningString = "(scanning...)"
	}

	// compute the average throughput for the last time interval
	bytesInMb := float64(float64(summary.BytesOverWire-cca.intervalBytesTransferred) / float64(1024*1024))
	timeElapsed := time.Since(cca.intervalStartTime).Seconds()
	throughPut := common.Iffloat64(timeElapsed != 0, bytesInMb/timeElapsed, 0) * 8

	// reset the interval timer and byte count
	cca.intervalStartTime = time.Now()
	cca.intervalBytesTransferred = summary.BytesOverWire

	// As there would be case when no bits sent from local, e.g. service side copy, when throughput = 0, hide it.
	if throughPut == 0 {
		glcm.Progress(fmt.Sprintf("%v Done, %v Failed, %v Pending, %v Skipped, %v Total%s",
			summary.TransfersCompleted,
			summary.TransfersFailed,
			summary.TotalTransfers-(summary.TransfersCompleted+summary.TransfersFailed+summary.TransfersSkipped),
			summary.TransfersSkipped,
			summary.TotalTransfers,
			scanningString))
	} else {
		glcm.Progress(fmt.Sprintf("%v Done, %v Failed, %v Pending, %v Skipped %v Total %s, 2-sec Throughput (Mb/s): %v",
			summary.TransfersCompleted,
			summary.TransfersFailed,
			summary.TotalTransfers-(summary.TransfersCompleted+summary.TransfersFailed+summary.TransfersSkipped),
			summary.TransfersSkipped, summary.TotalTransfers, scanningString, ste.ToFixed(throughPut, 4)))
	}
}

func isStdinPipeIn() (bool, error) {
	// check the Stdin to see if we are uploading or downloading
	info, err := os.Stdin.Stat()
	if err != nil {
		return false, fmt.Errorf("fatal: failed to read from Stdin due to error: %s", err)
	}

	return info.Mode()&os.ModeNamedPipe != 0, nil
}

// TODO check file size, max is 4.75TB
func init() {
	raw := rawCopyCmdArgs{}

	// cpCmd represents the cp command
	cpCmd := &cobra.Command{
		Use:        "copy [source] [destination]",
		Aliases:    []string{"cp", "c"},
		SuggestFor: []string{"cpy", "cy", "mv"}, //TODO why does message appear twice on the console
		Short:      "Move data between two places",
		Long: `
Copy(cp) moves data between two places. Local <=> Azure Data Lake Storage Gen2 are the only scenarios officially supported at the moment.
Please refer to the examples for more information.
`,
		Example: `Upload a single file:
  - azcopy cp "/path/to/file.txt" "https://[account].dfs.core.windows.net/[existing-filesystem]/[path/to/destination/directory/or/file]"

Upload an entire directory:
  - azcopy cp "/path/to/dir" "https://[account].dfs.core.windows.net/[existing-filesystem]/[path/to/destination/directory]" --recursive=true

Upload files using wildcards:
  - azcopy cp "/path/*foo/*bar/*.pdf" "https://[account].dfs.core.windows.net/[existing-filesystem]/[path/to/destination/directory]"

Upload files and/or directories using wildcards:
  - azcopy cp "/path/*foo/*bar*" "https://[account].dfs.core.windows.net/[existing-filesystem]/[path/to/destination/directory]" --recursive=true

Download a single file:
  - azcopy cp "https://[account].dfs.core.windows.net/[existing-filesystem]/[path/to/source/file]" "/path/to/file.txt"

Download an entire directory:
  - azcopy cp "https://[account].dfs.core.windows.net/[existing-filesystem]/[path/to/source/dir]" "/path/to/file.txt" --recursive=true
`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 { // redirection
				if stdinPipeIn, err := isStdinPipeIn(); stdinPipeIn == true {
					raw.src = pipeLocation
					raw.dst = args[0]
				} else {
					if err != nil {
						return fmt.Errorf("fatal: failed to read from Stdin due to error: %s", err)
					} else {
						raw.src = args[0]
						raw.dst = pipeLocation
					}
				}
			} else if len(args) == 2 { // normal copy
				raw.src = args[0]
				raw.dst = args[1]
			} else {
				return errors.New("wrong number of arguments, please refer to the help page on usage of this command")
			}
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			cooked, err := raw.cook()
			if err != nil {
				glcm.Exit("failed to parse user input due to error: "+err.Error(), common.EExitCode.Error())
			}
			cooked.commandString = copyHandlerUtil{}.ConstructCommandStringFromArgs()
			err = cooked.process()
			if err != nil {
				glcm.Exit("failed to perform copy command due to error: "+err.Error(), common.EExitCode.Error())
			}

			glcm.SurrenderControl()
		},
	}
	rootCmd.AddCommand(cpCmd)

	// define the flags relevant to the cp command
	// Visible flags
	cpCmd.PersistentFlags().Uint32Var(&raw.blockSize, "block-size", 0, "use this block(chunk) size when uploading/downloading to/from Azure Storage")
	cpCmd.PersistentFlags().BoolVar(&raw.forceWrite, "overwrite", true, "overwrite the conflicting files/blobs at the destination if this flag is set to true")
	cpCmd.PersistentFlags().StringVar(&raw.logVerbosity, "log-level", "INFO", "define the log verbosity for the log file, available levels: DEBUG, INFO, WARNING, ERROR, PANIC, and FATAL")
	cpCmd.PersistentFlags().BoolVar(&raw.recursive, "recursive", false, "look into sub-directories recursively when uploading from local file system")
	cpCmd.PersistentFlags().StringVar(&raw.output, "output", "text", "format of the command's output, the choices include: text, json")

	// hidden filters
	cpCmd.PersistentFlags().StringVar(&raw.include, "include", "", "Filter: only include these files when copying. "+
		"Support use of *. More than one file are separated by ';'")
	cpCmd.PersistentFlags().StringVar(&raw.exclude, "exclude", "", "Filter: Exclude these files when copying. Support use of *.")
	cpCmd.PersistentFlags().BoolVar(&raw.followSymlinks, "follow-symlinks", false, "Filter: Follow symbolic links when uploading from local file system.")
	cpCmd.PersistentFlags().BoolVar(&raw.withSnapshots, "with-snapshots", false, "Filter: Include the snapshots. Only valid when the source is blobs.")

	// hidden options
	cpCmd.PersistentFlags().StringVar(&raw.blockBlobTier, "block-blob-tier", "None", "Upload block blob to Azure Storage using this blob tier.")
	cpCmd.PersistentFlags().StringVar(&raw.pageBlobTier, "page-blob-tier", "None", "Upload page blob to Azure Storage using this blob tier.")
	cpCmd.PersistentFlags().StringVar(&raw.metadata, "metadata", "", "Upload to Azure Storage with these key-value pairs as metadata.")
	cpCmd.PersistentFlags().StringVar(&raw.contentType, "content-type", "", "Specifies content type of the file. Implies no-guess-mime-type.")
	cpCmd.PersistentFlags().StringVar(&raw.fromTo, "fromTo", "", "Specifies the source destination combination. For Example: LocalBlob, BlobLocal, LocalBlobFS")
	cpCmd.PersistentFlags().StringVar(&raw.contentEncoding, "content-encoding", "", "Upload to Azure Storage using this content encoding.")
	cpCmd.PersistentFlags().BoolVar(&raw.noGuessMimeType, "no-guess-mime-type", false, "This sets the content-type based on the extension of the file.")
	cpCmd.PersistentFlags().BoolVar(&raw.preserveLastModifiedTime, "preserve-last-modified-time", false, "Only available when destination is file system.")
	cpCmd.PersistentFlags().BoolVar(&raw.background, "background-op", false, "true if user has to perform the operations as a background operation")
	cpCmd.PersistentFlags().BoolVar(&raw.cancelFromStdin, "cancel-from-stdin", false, "true if user wants to cancel the process by passing 'cancel' "+
		"to the standard input. This is mostly used when the application is spawned by another process.")
	cpCmd.PersistentFlags().StringVar(&raw.acl, "acl", "", "Access conditions to be used when uploading/downloading from Azure Storage.")

	// oauth options
	cpCmd.PersistentFlags().BoolVar(&raw.useInteractiveOAuthUserCredential, "oauth-user", false, "Use OAuth user credential and do interactive login.")
	cpCmd.PersistentFlags().StringVar(&raw.tenantID, "tenant-id", common.DefaultTenantID, "Tenant id to use for OAuth user interactive login.")
	cpCmd.PersistentFlags().StringVar(&raw.aadEndpoint, "aad-endpoint", common.DefaultActiveDirectoryEndpoint, "Azure active directory endpoint to use for OAuth user interactive login.")

	// hide flags not relevant to BFS
	// TODO remove after preview release
	cpCmd.PersistentFlags().MarkHidden("include")
	cpCmd.PersistentFlags().MarkHidden("exclude")
	//cpCmd.PersistentFlags().MarkHidden("follow-symlinks")
	cpCmd.PersistentFlags().MarkHidden("with-snapshots")
	cpCmd.PersistentFlags().MarkHidden("output")

	cpCmd.PersistentFlags().MarkHidden("block-blob-tier")
	cpCmd.PersistentFlags().MarkHidden("page-blob-tier")
	cpCmd.PersistentFlags().MarkHidden("metadata")
	cpCmd.PersistentFlags().MarkHidden("content-type")
	cpCmd.PersistentFlags().MarkHidden("content-encoding")
	cpCmd.PersistentFlags().MarkHidden("no-guess-mime-type")
	cpCmd.PersistentFlags().MarkHidden("preserve-last-modified-time")
	cpCmd.PersistentFlags().MarkHidden("background-op")
	cpCmd.PersistentFlags().MarkHidden("fromTo")
	cpCmd.PersistentFlags().MarkHidden("acl")
	cpCmd.PersistentFlags().MarkHidden("stdIn-enable")

	// hide oauth feature temporarily
	cpCmd.PersistentFlags().MarkHidden("oauth-user")
	cpCmd.PersistentFlags().MarkHidden("tenant-id")
	cpCmd.PersistentFlags().MarkHidden("aad-endpoint")
}
