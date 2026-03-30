package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"sigs.k8s.io/prow/pkg/config"
	"sigs.k8s.io/prow/pkg/pluginhelp"

	"kubevirt.io/project-infra/external-plugins/label-detector/plugin/handler"
	"kubevirt.io/project-infra/external-plugins/label-detector/plugin/server"

	"github.com/sirupsen/logrus"
	"k8s.io/test-infra/pkg/flagutil"
	"sigs.k8s.io/prow/pkg/config/secret"
	prowflagutil "sigs.k8s.io/prow/pkg/flagutil"
	"sigs.k8s.io/prow/pkg/interrupts"
	"sigs.k8s.io/prow/pkg/pluginhelp/externalplugins"
)

type options struct {
	dryRun         bool
	hmacSecretFile string
	endpoint       string
	port           int
	github         prowflagutil.GitHubOptions
}

func (o *options) validate() {
	var errs []error
	err := o.github.Validate(o.dryRun)
	if err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		for _, err := range errs {
			logrus.WithError(err).Error("entry validation failure")
		}
		logrus.Fatalf("Arguments validation failed!")
	}
}

func gatherOptions() *options {
	o := &options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fs.IntVar(&o.port,
		"port",
		9900,
		"Port to listen on.")
	fs.StringVar(&o.endpoint,
		"endpoint",
		"/",
		"Endpoint to listen on.")
	fs.BoolVar(&o.dryRun,
		"dry-run",
		false,
		"If set, run in dry-run mode.")
	fs.StringVar(&o.hmacSecretFile,
		"hmac-secret-file",
		"/etc/webhook/hmac",
		"Path to the file containing the GitHub HMAC secret.")
	for _, group := range []flagutil.OptionGroup{&o.github} {
		group.AddFlags(fs)
	}
	fs.Parse(os.Args[1:])
	return o
}

func main() {
	opts := gatherOptions()
	opts.validate()

	logger := setupLogger()
	logger.Infoln("Setting up label-detector server")

	err := secret.Add(opts.github.TokenPath, opts.hmacSecretFile)
	mustSucceed(err, "Failed to start secrets agent")

	githubClient, err := opts.github.GitHubClient(opts.dryRun)
	mustSucceed(err, "Could not instantiate github client")

	// Create conformance detector
	conformanceDetector := NewConformanceDetector(logger.WithField("component", "conformance-detector"))

	// Create event handler
	eventsHandler := handler.NewGitHubEventsHandler(
		nil,
		logger,
		githubClient,
		conformanceDetector)

	// Create a token generator function
	tokenGen := secret.GetTokenGenerator(opts.hmacSecretFile)

	// Create events server with HMAC verification
	eventsServer := server.NewGitHubEventsServer(tokenGen, eventsHandler)

	// Setup HTTP server
	serverMux := http.NewServeMux()
	serverMux.Handle(opts.endpoint, eventsServer)
	srv := &http.Server{Addr: fmt.Sprintf(":%d", opts.port), Handler: serverMux}

	interrupts.ListenAndServe(srv, 5*time.Second)
	logger.Infoln("Label-detector server is listening on port:", opts.port)

	// Serve plugin help
	externalplugins.ServeExternalPluginHelp(serverMux, logger.WithField("plugin-help", ""), helpProvider)

	interrupts.WaitForGracefulShutdown()
	logger.Println("Label-detector server was gracefully shut down")
}

func helpProvider(_ []config.OrgRepo) (*pluginhelp.PluginHelp, error) {
	pluginHelp := &pluginhelp.PluginHelp{
		Description: `The Label-detector plugin automatically detects when Conformance tests are modified in a pull request.`,
	}
	return pluginHelp, nil
}

func mustSucceed(err error, message string) {
	if err != nil {
		logrus.WithError(err).Fatal(message)
	}
}

func setupLogger() *logrus.Logger {
	l := logrus.New()
	l.SetFormatter(&logrus.TextFormatter{FullTimestamp: true, TimestampFormat: time.RFC1123Z})
	l.SetLevel(logrus.DebugLevel)
	l.SetOutput(os.Stdout)
	return l
}
