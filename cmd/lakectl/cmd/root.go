package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/deepmap/oapi-codegen/pkg/securityprovider"
	"github.com/mitchellh/go-homedir"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/treeverse/lakefs/cmd/lakectl/cmd/config"
	"github.com/treeverse/lakefs/pkg/api"
	"github.com/treeverse/lakefs/pkg/version"
)

const (
	ConfigAccessKeyID        = "credentials.access_key_id"
	ConfigSecretAccessKey    = "credentials.secret_access_key"
	ConfigServerEndpointURL  = "server.endpoint_url"
	DefaultServerEndpointURL = "http://127.0.0.1:8000"

	DefaultMaxIdleConnsPerHost = 1000
)

var (
	cfgFile string
	cfg     *config.Config
)

// rootCmd represents the base command when called without any sub-commands
var rootCmd = &cobra.Command{
	Use:   "lakectl",
	Short: "A cli tool to explore manage and work with lakeFS",
	Long: `lakeFS is data lake management solution, allowing Git-like semantics over common object stores

lakectl is a CLI tool allowing exploration and manipulation of a lakeFS environment`,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		if noColorRequested {
			DisableColors()
		}
		if cmd == configCmd {
			return
		}

		if errors.As(cfg.Err(), &viper.ConfigFileNotFoundError{}) {
			if cfgFile == "" {
				// if the config file wasn't provided, try to run using the default values + env vars
				return
			}
			// specific message in case the file isn't found
			DieFmt("config file not found, please run \"lakectl config\" to create one\n%s\n", cfg.Err())
		} else if cfg.Err() != nil {
			// other errors while reading the config file
			DieFmt("error reading configuration file: %v", cfg.Err())
		}
	},
	Version: version.Version,
}

func getClient() api.ClientWithResponsesInterface {
	// override MaxIdleConnsPerHost to allow highly concurrent access to our API client.
	// This is done to avoid accumulating many sockets in `TIME_WAIT` status that were closed
	// only to be immediately reopened.
	// see: https://stackoverflow.com/a/39834253
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = DefaultMaxIdleConnsPerHost

	accessKeyID := cfg.Credentials.AccessKeyID
	secretAccessKey := cfg.Credentials.SecretAccessKey
	basicAuthProvider, err := securityprovider.NewSecurityProviderBasicAuth(accessKeyID, secretAccessKey)
	if err != nil {
		DieErr(err)
	}

	serverEndpoint := cfg.Server.EndpointURL
	u, err := url.Parse(serverEndpoint)
	if err != nil {
		DieErr(err)
	}
	// if no uri to api is set in configuration - set the default
	if u.Path == "" || u.Path == "/" {
		serverEndpoint = strings.TrimRight(serverEndpoint, "/") + api.BaseURL
	}

	client, err := api.NewClientWithResponses(
		serverEndpoint,
		api.WithRequestEditorFn(basicAuthProvider.Intercept),
	)
	if err != nil {
		Die(fmt.Sprintf("could not initialize API client: %s", err), 1)
	}
	return client
}

// isSeekable returns true if f.Seek appears to work.
func isSeekable(f io.Seeker) bool {
	_, err := f.Seek(0, io.SeekCurrent)
	return err == nil // a little naive, but probably good enough for its purpose
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if noColorRequested {
		DisableColors()
	}
	err := rootCmd.Execute()
	if err != nil {
		DieErr(err)
	}
}

// ParseDocument parses the contents of filename into dest, which
// should be a JSON-deserializable struct.  If filename is "-" it
// reads standard input.  If any errors occur it dies with an error
// message containing fileTitle.
func ParseDocument(dest interface{}, filename string, fileTitle string) {
	var (
		fp  io.ReadCloser
		err error
	)
	if filename == "-" {
		fp = os.Stdin
	} else {
		if fp, err = os.Open(filename); err != nil {
			DieFmt("open %s %s for read: %v", fileTitle, filename, err)
		}
		defer func() {
			_ = fp.Close()
		}()
	}
	if err := json.NewDecoder(fp).Decode(dest); err != nil {
		DieFmt("could not parse %s document: %v", fileTitle, err)
	}
}

//nolint:gochecknoinits
func init() {
	// Here you will define your flags and configuration settings.
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "config file (default is $HOME/.lakectl.yaml)")
	rootCmd.PersistentFlags().BoolVar(&noColorRequested, "no-color", false, "don't use fancy output colors (default when not attached to an interactive terminal)")
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		// Find home directory.
		home, err := homedir.Dir()
		if err != nil {
			DieErr(err)
		}

		// Search config in home directory with name ".lakefs" (without extension).
		viper.AddConfigPath(home)
		viper.SetConfigType("yaml")
		viper.SetConfigName(".lakectl")
	}

	viper.SetEnvPrefix("LAKECTL")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_")) // support nested config
	viper.AutomaticEnv()                                   // read in environment variables that match

	// Configuration defaults
	viper.SetDefault(ConfigServerEndpointURL, DefaultServerEndpointURL)

	cfg = config.ReadConfig()
}
