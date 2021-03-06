/*
 * Minio Cloud Storage, (C) 2015, 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"runtime"

	"github.com/minio/cli"
)

var serverFlags = []cli.Flag{
	cli.StringFlag{
		Name:  "address",
		Value: ":9000",
		Usage: `Bind to a specific IP:PORT. Defaults to ":9000".`,
	},
}

var serverCmd = cli.Command{
	Name:   "server",
	Usage:  "Start object storage server.",
	Flags:  append(serverFlags, globalFlags...),
	Action: serverMain,
	CustomHelpTemplate: `NAME:
  minio {{.Name}} - {{.Usage}}

USAGE:
  minio {{.Name}} [FLAGS] PATH [PATH...]

FLAGS:
  {{range .Flags}}{{.}}
  {{end}}
ENVIRONMENT VARIABLES:
  ACCESS:
     MINIO_ACCESS_KEY: Custom username or access key of 5 to 20 characters in length.
     MINIO_SECRET_KEY: Custom password or secret key of 8 to 40 characters in length.

  BROWSER:
     MINIO_BROWSER: To disable web browser access, set this value to "off".

EXAMPLES:
  1. Start minio server on "/home/shared" directory.
      $ minio {{.Name}} /home/shared

  2. Start minio server bound to a specific IP:PORT.
      $ minio {{.Name}} --address 192.168.1.101:9000 /home/shared

  3. Start erasure coded minio server on a 12 disks server.
      $ minio {{.Name}} /mnt/export1/ /mnt/export2/ /mnt/export3/ /mnt/export4/ \
          /mnt/export5/ /mnt/export6/ /mnt/export7/ /mnt/export8/ /mnt/export9/ \
          /mnt/export10/ /mnt/export11/ /mnt/export12/

  4. Start erasure coded distributed minio server on a 4 node setup with 1 drive each. Run following commands on all the 4 nodes.
      $ export MINIO_ACCESS_KEY=minio
      $ export MINIO_SECRET_KEY=miniostorage
      $ minio {{.Name}} http://192.168.1.11/mnt/export/ http://192.168.1.12/mnt/export/ \
          http://192.168.1.13/mnt/export/ http://192.168.1.14/mnt/export/

`,
}

type serverCmdConfig struct {
	serverAddr   string
	endpoints    []*url.URL
	storageDisks []StorageAPI
}

// Parse an array of end-points (from the command line)
func parseStorageEndpoints(eps []string) (endpoints []*url.URL, err error) {
	for _, ep := range eps {
		if ep == "" {
			return nil, errInvalidArgument
		}
		var u *url.URL
		u, err = url.Parse(ep)
		if err != nil {
			return nil, err
		}
		if u.Host != "" {
			_, port, err := net.SplitHostPort(u.Host)
			// Ignore the missing port error as the default port can be globalMinioPort.
			if err != nil && !strings.Contains(err.Error(), "missing port in address") {
				return nil, err
			}

			if globalMinioHost == "" {
				// For ex.: minio server host1:port1 host2:port2...
				// we return error as port is configurable only
				// using "--address :port"
				if port != "" {
					return nil, fmt.Errorf("Invalid Argument %s, port configurable using --address :<port>", u.Host)
				}
				u.Host = net.JoinHostPort(u.Host, globalMinioPort)
			} else {
				// For ex.: minio server --address host:port host1:port1 host2:port2...
				// i.e if "--address host:port" is specified
				// port info in u.Host is mandatory else return error.
				if port == "" {
					return nil, fmt.Errorf("Invalid Argument %s, port mandatory when --address <host>:<port> is used", u.Host)
				}
			}
		}
		endpoints = append(endpoints, u)
	}
	return endpoints, nil
}

// initServerConfig initialize server config.
func initServerConfig(c *cli.Context) {
	// Create certs path.
	err := createCertsPath()
	fatalIf(err, "Unable to create \"certs\" directory.")

	// Load user supplied root CAs
	loadRootCAs()

	// When credentials inherited from the env, server cmd has to save them in the disk
	if os.Getenv("MINIO_ACCESS_KEY") != "" && os.Getenv("MINIO_SECRET_KEY") != "" {
		// Env credentials are already loaded in serverConfig, just save in the disk
		err = serverConfig.Save()
		fatalIf(err, "Unable to save credentials in the disk.")
	}

	// Set maxOpenFiles, This is necessary since default operating
	// system limits of 1024, 2048 are not enough for Minio server.
	setMaxOpenFiles()

	// Set maxMemory, This is necessary since default operating
	// system limits might be changed and we need to make sure we
	// do not crash the server so the set the maxCacheSize appropriately.
	setMaxMemory()

	// Do not fail if this is not allowed, lower limits are fine as well.
}

// Validate if input disks are sufficient for initializing XL.
func checkSufficientDisks(eps []*url.URL) error {
	// Verify total number of disks.
	total := len(eps)
	if total > maxErasureBlocks {
		return errXLMaxDisks
	}
	if total < minErasureBlocks {
		return errXLMinDisks
	}

	// isEven function to verify if a given number if even.
	isEven := func(number int) bool {
		return number%2 == 0
	}

	// Verify if we have even number of disks.
	// only combination of 4, 6, 8, 10, 12, 14, 16 are supported.
	if !isEven(total) {
		return errXLNumDisks
	}

	// Success.
	return nil
}

// Returns if slice of disks is a distributed setup.
func isDistributedSetup(eps []*url.URL) bool {
	// Validate if one the disks is not local.
	for _, ep := range eps {
		if !isLocalStorage(ep) {
			// One or more disks supplied as arguments are
			// not attached to the local node.
			return true
		}
	}
	return false
}

// Check if endpoint is in expected syntax by valid scheme/path across all platforms.
func checkEndpointURL(endpointURL *url.URL) (err error) {
	// applicable to all OS.
	if endpointURL.Scheme == "" || endpointURL.Scheme == "http" || endpointURL.Scheme == "https" {
		urlPath := path.Clean(endpointURL.Path)
		if urlPath == "" || urlPath == "." || urlPath == "/" || urlPath == `\` {
			err = fmt.Errorf("Empty or root path is not allowed")
		}

		return err
	}

	// Applicable to Windows only.
	if runtime.GOOS == "windows" {
		// On Windows, endpoint can be a path with drive eg. C:\Export and its URL.Scheme is 'C'.
		// Check if URL.Scheme is a single letter alphabet to represent a drive.
		// Note: URL.Parse() converts scheme into lower case always.
		if len(endpointURL.Scheme) == 1 && endpointURL.Scheme[0] >= 'a' && endpointURL.Scheme[0] <= 'z' {
			// If endpoint is C:\ or C:\export, URL.Path does not have path information like \ or \export
			// hence we directly work with endpoint.
			urlPath := strings.SplitN(path.Clean(endpointURL.String()), ":", 2)[1]
			if urlPath == "" || urlPath == "." || urlPath == "/" || urlPath == `\` {
				err = fmt.Errorf("Empty or root path is not allowed")
			}

			return err
		}
	}

	return fmt.Errorf("Invalid scheme")
}

// Check if endpoints are in expected syntax by valid scheme/path across all platforms.
func checkEndpointsSyntax(eps []*url.URL, disks []string) error {
	for i, u := range eps {
		if err := checkEndpointURL(u); err != nil {
			return fmt.Errorf("%s: %s (%s)", err.Error(), u.Path, disks[i])
		}
	}

	return nil
}

// Make sure all the command line parameters are OK and exit in case of invalid parameters.
func checkServerSyntax(c *cli.Context) {
	serverAddr := c.String("address")

	host, portStr, err := net.SplitHostPort(serverAddr)
	fatalIf(err, "Unable to parse %s.", serverAddr)

	// Verify syntax for all the XL disks.
	disks := c.Args()
	endpoints, err := parseStorageEndpoints(disks)
	fatalIf(err, "Unable to parse storage endpoints %s", strings.Join(disks, " "))

	// Validate if endpoints follow the expected syntax.
	err = checkEndpointsSyntax(endpoints, disks)
	fatalIf(err, "Invalid endpoints found %s", strings.Join(disks, " "))

	// Validate for duplicate endpoints are supplied.
	err = checkDuplicateEndpoints(endpoints)
	fatalIf(err, "Duplicate entries in %s", strings.Join(disks, " "))

	if len(endpoints) > 1 {
		// Validate if we have sufficient disks for XL setup.
		err = checkSufficientDisks(endpoints)
		fatalIf(err, "Invalid number of disks supplied.")
	} else {
		// Validate if we have invalid disk for FS setup.
		if endpoints[0].Host != "" && endpoints[0].Scheme != "" {
			fatalIf(errInvalidArgument, "%s, FS setup expects a filesystem path", endpoints[0])
		}
	}

	if !isDistributedSetup(endpoints) {
		// for FS and singlenode-XL validation is done, return.
		return
	}

	// Rest of the checks applies only to distributed XL setup.
	if host != "" {
		// We are here implies --address host:port is passed, hence the user is trying
		// to run one minio process per export disk.
		if portStr == "" {
			fatalIf(errInvalidArgument, "Port missing, Host:Port should be specified for --address")
		}
		foundCnt := 0
		for _, ep := range endpoints {
			if ep.Host == serverAddr {
				foundCnt++
			}
		}
		if foundCnt == 0 {
			// --address host:port should be available in the XL disk list.
			fatalIf(errInvalidArgument, "%s is not available in %s", serverAddr, strings.Join(disks, " "))
		}
		if foundCnt > 1 {
			// --address host:port should match exactly one entry in the XL disk list.
			fatalIf(errInvalidArgument, "%s matches % entries in %s", serverAddr, foundCnt, strings.Join(disks, " "))
		}
	}

	for _, ep := range endpoints {
		if ep.Scheme == "https" && !globalIsSSL {
			// Certificates should be provided for https configuration.
			fatalIf(errInvalidArgument, "Certificates not provided for secure configuration")
		}
	}
}

// Checks if any of the endpoints supplied is local to this server.
func isAnyEndpointLocal(eps []*url.URL) bool {
	anyLocalEp := false
	for _, ep := range eps {
		if isLocalStorage(ep) {
			anyLocalEp = true
			break
		}
	}
	return anyLocalEp
}

// Returned when there are no ports.
var errEmptyPort = errors.New("Port cannot be empty or '0', please use `--address` to pick a specific port")

// Convert an input address of form host:port into, host and port, returns if any.
func getHostPort(address string) (host, port string, err error) {
	// Check if requested port is available.
	host, port, err = net.SplitHostPort(address)
	if err != nil {
		return "", "", err
	}

	// Empty ports.
	if port == "0" || port == "" {
		// Port zero or empty means use requested to choose any freely available
		// port. Avoid this since it won't work with any configured clients,
		// can lead to serious loss of availability.
		return "", "", errEmptyPort
	}

	// Parse port.
	if _, err = strconv.Atoi(port); err != nil {
		return "", "", err
	}

	// Check if port is available.
	if err = checkPortAvailability(port); err != nil {
		return "", "", err
	}

	// Success.
	return host, port, nil
}

// serverMain handler called for 'minio server' command.
func serverMain(c *cli.Context) {
	if !c.Args().Present() || c.Args().First() == "help" {
		cli.ShowCommandHelpAndExit(c, "server", 1)
	}

	// Initialization routine, such as config loading, enable logging, ..
	minioInit(c)

	// Check for minio updates from dl.minio.io
	checkUpdate()

	// Server address.
	serverAddr := c.String("address")

	var err error
	globalMinioHost, globalMinioPort, err = getHostPort(serverAddr)
	fatalIf(err, "Unable to extract host and port %s", serverAddr)

	// Check server syntax and exit in case of errors.
	// Done after globalMinioHost and globalMinioPort is set as parseStorageEndpoints()
	// depends on it.
	checkServerSyntax(c)

	// Disks to be used in server init.
	endpoints, err := parseStorageEndpoints(c.Args())
	fatalIf(err, "Unable to parse storage endpoints %s", c.Args())

	// Should exit gracefully if none of the endpoints passed
	// as command line args are local to this server.
	if !isAnyEndpointLocal(endpoints) {
		fatalIf(errInvalidArgument, "None of the disks passed as command line args are local to this server.")
	}

	// Sort endpoints for consistent ordering across multiple
	// nodes in a distributed setup. This is to avoid format.json
	// corruption if the disks aren't supplied in the same order
	// on all nodes.
	sort.Sort(byHostPath(endpoints))

	storageDisks, err := initStorageDisks(endpoints)
	fatalIf(err, "Unable to initialize storage disk(s).")

	// Cleanup objects that weren't successfully written into the namespace.
	fatalIf(houseKeeping(storageDisks), "Unable to purge temporary files.")

	// Initialize server config.
	initServerConfig(c)

	// First disk argument check if it is local.
	firstDisk := isLocalStorage(endpoints[0])

	// Check if endpoints are part of distributed setup.
	globalIsDistXL = isDistributedSetup(endpoints)

	// Configure server.
	srvConfig := serverCmdConfig{
		serverAddr:   serverAddr,
		endpoints:    endpoints,
		storageDisks: storageDisks,
	}

	// Configure server.
	handler, err := configureServerHandler(srvConfig)
	fatalIf(err, "Unable to configure one of server's RPC services.")

	// Set nodes for dsync for distributed setup.
	if globalIsDistXL {
		fatalIf(initDsyncNodes(endpoints), "Unable to initialize distributed locking")
	}

	// Initialize name space lock.
	initNSLock(globalIsDistXL)

	// Initialize a new HTTP server.
	apiServer := NewServerMux(serverAddr, handler)

	// Set the global minio addr for this server.
	globalMinioAddr = getLocalAddress(srvConfig)

	// Determine API endpoints where we are going to serve the S3 API from.
	apiEndPoints, err := finalizeAPIEndpoints(apiServer.Server)
	fatalIf(err, "Unable to finalize API endpoints for %s", apiServer.Server.Addr)

	// Set the global API endpoints value.
	globalAPIEndpoints = apiEndPoints

	// Initialize S3 Peers inter-node communication
	initGlobalS3Peers(endpoints)

	// Initialize Admin Peers inter-node communication
	initGlobalAdminPeers(endpoints)

	// Start server, automatically configures TLS if certs are available.
	go func() {
		cert, key := "", ""
		if globalIsSSL {
			cert, key = mustGetCertFile(), mustGetKeyFile()
		}
		fatalIf(apiServer.ListenAndServe(cert, key), "Failed to start minio server.")
	}()

	// Wait for formatting of disks.
	formattedDisks, err := waitForFormatDisks(firstDisk, endpoints, storageDisks)
	fatalIf(err, "formatting storage disks failed")

	// Once formatted, initialize object layer.
	newObject, err := newObjectLayer(formattedDisks)
	fatalIf(err, "intializing object layer failed")

	globalObjLayerMutex.Lock()
	globalObjectAPI = newObject
	globalObjLayerMutex.Unlock()

	// Prints the formatted startup message once object layer is initialized.
	printStartupMessage(apiEndPoints)

	// Waits on the server.
	<-globalServiceDoneCh
}
