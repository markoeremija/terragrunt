package tf

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gruntwork-io/terragrunt/options"
	"github.com/gruntwork-io/terragrunt/pkg/log"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/go-getter"
	safetemp "github.com/hashicorp/go-safetemp"
	svchost "github.com/hashicorp/terraform-svchost"

	"github.com/gruntwork-io/terragrunt/internal/errors"
	"github.com/gruntwork-io/terragrunt/tf/cliconfig"
	"github.com/gruntwork-io/terragrunt/util"
)

// httpClient is the default client to be used by HttpGetters.
var httpClient = cleanhttp.DefaultClient()

// Constants relevant to the module registry
const (
	defaultRegistryDomain   = "registry.terraform.io"
	defaultOtRegistryDomain = "registry.opentofu.org"
	serviceDiscoveryPath    = "/.well-known/terraform.json"
	versionQueryKey         = "version"
	authTokenEnvName        = "TG_TF_REGISTRY_TOKEN"
	defaultRegistryEnvName  = "TG_TF_DEFAULT_REGISTRY_HOST"
)

// RegistryServicePath is a struct for extracting the modules service path in the Registry.
type RegistryServicePath struct {
	ModulesPath string `json:"modules.v1"`
}

// RegistryGetter is a Getter (from go-getter) implementation that will download from the terraform module
// registry. This supports getter URLs encoded in the following manner:
//
// tfr://REGISTRY_DOMAIN/MODULE_PATH?version=VERSION
//
// Where the REGISTRY_DOMAIN is the terraform registry endpoint (e.g., registry.terraform.io), MODULE_PATH is the
// registry path for the module (e.g., terraform-aws-modules/vpc/aws), and VERSION is the specific version of the module
// to download (e.g., 2.2.0).
//
// This protocol will use the Module Registry Protocol (documented at
// https://www.terraform.io/docs/internals/module-registry-protocol.html) to lookup the module source URL and download
// it.
//
// Authentication to private module registries is handled via environment variables. The authorization API token is
// expected to be provided to Terragrunt via the TG_TF_REGISTRY_TOKEN environment variable. This token can be any
// registry API token generated on Terraform Cloud / Enterprise.
//
// MAINTAINER'S NOTE: Ideally we implement the full credential system that terraform uses as part of `terraform login`,
// but all the relevant packages are internal to the terraform repository, thus making it difficult to use as a
// library. For now, we keep things simple by supporting providing tokens via env vars and in the future, we can
// consider implementing functionality to load credentials from terraform.
// GH issue: https://github.com/gruntwork-io/terragrunt/issues/1771
//
// MAINTAINER'S NOTE: Ideally we can support a shorthand notation that omits the tfr:// protocol to detect that it is
// referring to a terraform registry, but this requires implementing a complex detector and ensuring it has precedence
// over the file detector. We deferred the implementation for that to a future release.
// GH issue: https://github.com/gruntwork-io/terragrunt/issues/1772
type RegistryGetter struct {
	client            *getter.Client
	TerragruntOptions *options.TerragruntOptions
	Logger            log.Logger
}

// SetClient allows the getter to know what getter client (different from the underlying HTTP client) to use for
// progress tracking.
func (tfrGetter *RegistryGetter) SetClient(client *getter.Client) {
	tfrGetter.client = client
}

// Context returns the go context to use for the underlying fetch routines. This depends on what client is set.
func (tfrGetter *RegistryGetter) Context() context.Context {
	if tfrGetter == nil || tfrGetter.client == nil {
		return context.Background()
	}

	return tfrGetter.client.Ctx
}

// registryDomain returns the default registry domain to use for the getter.
func (tfrGetter *RegistryGetter) registryDomain() string {
	return GetDefaultRegistryDomain(tfrGetter.TerragruntOptions)
}

// GetDefaultRegistryDomain returns the appropriate registry domain based on the terraform implementation and environment variables.
// This is the canonical function for determining which registry to use throughout Terragrunt.
func GetDefaultRegistryDomain(opts *options.TerragruntOptions) string {
	if opts == nil {
		return defaultRegistryDomain
	}

	// if is set TG_TF_DEFAULT_REGISTRY env var, use it as default registry
	if defaultRegistry := os.Getenv(defaultRegistryEnvName); defaultRegistry != "" {
		return defaultRegistry
	}
	// if binary is set to use OpenTofu registry, use OpenTofu as default registry
	if opts.TerraformImplementation == options.OpenTofuImpl {
		return defaultOtRegistryDomain
	}

	return defaultRegistryDomain
}

// ClientMode returns the download mode based on the given URL. Since this getter is designed around the Terraform
// module registry, we always use Dir mode so that we can download the full Terraform module.
func (tfrGetter *RegistryGetter) ClientMode(u *url.URL) (getter.ClientMode, error) {
	return getter.ClientModeDir, nil
}

// Get is the main routine to fetch the module contents specified at the given URL and download it to the dstPath.
// This routine assumes that the srcURL points to the Terraform registry URL, with the Path configured to the module
// path encoded as `:namespace/:name/:system` as expected by the Terraform registry. Note that the URL query parameter
// must have the `version` key to specify what version to download.
func (tfrGetter *RegistryGetter) Get(dstPath string, srcURL *url.URL) error {
	ctx := tfrGetter.Context()

	registryDomain := srcURL.Host
	if registryDomain == "" {
		registryDomain = tfrGetter.registryDomain()
	}

	queryValues := srcURL.Query()
	modulePath, moduleSubDir := getter.SourceDirSubdir(srcURL.Path)

	versionList, hasVersion := queryValues[versionQueryKey]
	if !hasVersion {
		return errors.New(MalformedRegistryURLErr{reason: "missing version query"})
	}

	if len(versionList) != 1 {
		return errors.New(MalformedRegistryURLErr{reason: "more than one version query"})
	}

	version := versionList[0]

	moduleRegistryBasePath, err := GetModuleRegistryURLBasePath(ctx, tfrGetter.Logger, registryDomain)
	if err != nil {
		return err
	}

	moduleURL, err := BuildRequestURL(registryDomain, moduleRegistryBasePath, modulePath, version)
	if err != nil {
		return err
	}

	terraformGet, err := GetTerraformGetHeader(ctx, tfrGetter.Logger, *moduleURL)
	if err != nil {
		return err
	}

	downloadURL, err := GetDownloadURLFromHeader(*moduleURL, terraformGet)
	if err != nil {
		return err
	}

	// If there is a subdir component, then we download the root separately into a temporary directory, then copy over
	// the proper subdir. Note that we also have to take into account sub dirs in the original URL in addition to the
	// subdir component in the X-Terraform-Get download URL.
	source, subDir := getter.SourceDirSubdir(downloadURL)
	if subDir == "" && moduleSubDir == "" {
		var opts []getter.ClientOption
		if tfrGetter.client != nil {
			opts = tfrGetter.client.Options
		}

		return getter.Get(dstPath, source, opts...)
	}

	// We have a subdir, time to jump some hoops
	return tfrGetter.getSubdir(ctx, tfrGetter.Logger, dstPath, source, path.Join(subDir, moduleSubDir))
}

// GetFile is not implemented for the Terraform module registry Getter since the terraform module registry doesn't serve
// a single file.
func (tfrGetter *RegistryGetter) GetFile(dst string, src *url.URL) error {
	return errors.New(errors.New("GetFile is not implemented for the Terraform Registry Getter"))
}

// getSubdir downloads the source into the destination, but with the proper subdir.
func (tfrGetter *RegistryGetter) getSubdir(_ context.Context, l log.Logger, dstPath, sourceURL, subDir string) error {
	// Create a temporary directory to store the full source. This has to be a non-existent directory.
	tempdirPath, tempdirCloser, err := safetemp.Dir("", "getter")
	if err != nil {
		return err
	}
	defer func(tempdirCloser io.Closer) {
		err := tempdirCloser.Close()
		if err != nil {
			l.Warnf("Error closing temporary directory %s: %v", tempdirPath, err)
		}
	}(tempdirCloser)

	var opts []getter.ClientOption
	if tfrGetter.client != nil {
		opts = tfrGetter.client.Options
	}
	// Download that into the given directory
	if err := getter.Get(tempdirPath, sourceURL, opts...); err != nil {
		return errors.New(err)
	}

	// Process any globbing
	sourcePath, err := getter.SubdirGlob(tempdirPath, subDir)
	if err != nil {
		return errors.New(err)
	}

	// Make sure the subdir path actually exists
	if _, err := os.Stat(sourcePath); err != nil {
		details := fmt.Sprintf("could not stat download path %s (error: %s)", sourcePath, err)

		return errors.New(ModuleDownloadErr{sourceURL: sourceURL, details: details})
	}

	// Copy the subdirectory into our actual destination.
	if err := os.RemoveAll(dstPath); err != nil {
		return errors.New(err)
	}

	// Make the final destination
	const ownerWriteGlobalReadExecutePerms = 0755
	if err := os.MkdirAll(dstPath, ownerWriteGlobalReadExecutePerms); err != nil {
		return errors.New(err)
	}

	// We use a temporary manifest file here that is deleted at the end of this routine since we don't intend to come
	// back to it.
	manifestFname := ".tgmanifest"
	manifestPath := filepath.Join(dstPath, manifestFname)

	defer func(name string) {
		err := os.Remove(name)
		if err != nil {
			l.Warnf("Error removing temporary directory %s: %v", name, err)
		}
	}(manifestPath)

	return util.CopyFolderContentsWithFilter(l, sourcePath, dstPath, manifestFname, func(path string) bool { return true })
}

// GetModuleRegistryURLBasePath uses the service discovery protocol
// (https://www.terraform.io/docs/internals/remote-service-discovery.html)
// to figure out where the modules are stored. This will return the base
// path where the modules can be accessed
func GetModuleRegistryURLBasePath(ctx context.Context, logger log.Logger, domain string) (string, error) {
	sdURL := url.URL{
		Scheme: "https",
		Host:   domain,
		Path:   serviceDiscoveryPath,
	}

	bodyData, _, err := httpGETAndGetResponse(ctx, logger, sdURL)
	if err != nil {
		return "", err
	}

	var respJSON RegistryServicePath
	if err := json.Unmarshal(bodyData, &respJSON); err != nil {
		reason := fmt.Sprintf("Error parsing response body %s: %s", string(bodyData), err)

		return "", errors.New(ServiceDiscoveryErr{reason: reason})
	}

	return respJSON.ModulesPath, nil
}

// GetTerraformGetHeader makes an http GET call to the given registry URL and return the contents of location json
// body or the header X-Terraform-Get. This function will return an error if the response does not contain the header.
func GetTerraformGetHeader(ctx context.Context, logger log.Logger, url url.URL) (string, error) {
	body, header, err := httpGETAndGetResponse(ctx, logger, url)
	if err != nil {
		details := "error receiving HTTP data"

		return "", errors.New(ModuleDownloadErr{sourceURL: url.String(), details: details})
	}

	terraformGet := header.Get("X-Terraform-Get")
	if terraformGet != "" {
		return terraformGet, nil
	}

	// parse response from body as json
	var responseJSON map[string]string
	if err := json.Unmarshal(body, &responseJSON); err != nil {
		reason := fmt.Sprintf("Error parsing response body %s: %s", string(body), err)

		return "", errors.New(ModuleDownloadErr{sourceURL: url.String(), details: reason})
	}
	// get location value from responseJSON
	terraformGet = responseJSON["location"]
	if terraformGet != "" {
		return terraformGet, nil
	}

	if terraformGet == "" {
		details := "no source URL was returned in header X-Terraform-Get and in location response from download URL"

		return "", errors.New(ModuleDownloadErr{sourceURL: url.String(), details: details})
	}

	return terraformGet, nil
}

// GetDownloadURLFromHeader checks if the content of the X-Terraform-GET header contains the base url
// and prepends it if not
func GetDownloadURLFromHeader(moduleURL url.URL, terraformGet string) (string, error) {
	// If url from X-Terrafrom-Get Header seems to be a relative url,
	// append scheme and host from url used for getting the download url
	// because third-party registry implementations may not "know" their own absolute URLs if
	// e.g. they are running behind a reverse proxy frontend, or such.
	if strings.HasPrefix(terraformGet, "/") || strings.HasPrefix(terraformGet, "./") || strings.HasPrefix(terraformGet, "../") {
		relativePathURL, err := url.Parse(terraformGet)
		if err != nil {
			return "", errors.New(err)
		}

		terraformGetURL := moduleURL.ResolveReference(relativePathURL)
		terraformGet = terraformGetURL.String()
	}

	return terraformGet, nil
}

func applyHostToken(req *http.Request) (*http.Request, error) {
	cliCfg, err := cliconfig.LoadUserConfig()
	if err != nil {
		return nil, err
	}

	if creds := cliCfg.CredentialsSource().ForHost(svchost.Hostname(req.URL.Hostname())); creds != nil {
		creds.PrepareRequest(req)
	} else {
		// fall back to the TG_TF_REGISTRY_TOKEN
		authToken := os.Getenv(authTokenEnvName)
		if authToken != "" {
			req.Header.Add("Authorization", "Bearer "+authToken)
		}
	}

	return req, nil
}

// httpGETAndGetResponse is a helper function to make a GET request to the given URL using the http client. This
// function will then read the response and return the contents + the response header.
func httpGETAndGetResponse(ctx context.Context, logger log.Logger, getURL url.URL) ([]byte, *http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", getURL.String(), nil)
	if err != nil {
		return nil, nil, errors.New(err)
	}

	// Handle authentication via env var. Authentication is done by providing the registry token as a bearer token in
	// the request header.
	req, err = applyHostToken(req)
	if err != nil {
		return nil, nil, errors.New(err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, errors.New(err)
	}

	defer func() {
		err := resp.Body.Close()
		if err != nil {
			logger.Warnf("Error closing response body: %v", err)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, errors.New(RegistryAPIErr{url: getURL.String(), statusCode: resp.StatusCode})
	}

	bodyData, err := io.ReadAll(resp.Body)

	return bodyData, &resp.Header, errors.New(err)
}

// BuildRequestURL - create url to download module using moduleRegistryBasePath
func BuildRequestURL(registryDomain string, moduleRegistryBasePath string, modulePath string, version string) (*url.URL, error) {
	moduleRegistryBasePath = strings.TrimSuffix(moduleRegistryBasePath, "/")
	modulePath = strings.TrimSuffix(modulePath, "/")
	modulePath = strings.TrimPrefix(modulePath, "/")

	moduleFullPath := fmt.Sprintf("%s/%s/%s/download", moduleRegistryBasePath, modulePath, version)

	moduleURL, err := url.Parse(moduleFullPath)
	if err != nil {
		return nil, err
	}

	if moduleURL.Scheme != "" {
		return moduleURL, nil
	}

	return &url.URL{Scheme: "https", Host: registryDomain, Path: moduleFullPath}, nil
}
