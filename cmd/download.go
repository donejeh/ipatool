package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go"
	"github.com/majd/ipatool/v2/pkg/appstore"
	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"
)

// nolint:wrapcheck
func downloadCmd() *cobra.Command {
	return downloadCmdWithAppStore(func() appstore.AppStore { return dependencies.AppStore })
}

//nolint:wrapcheck
func downloadCmdWithAppStore(appStore func() appstore.AppStore) *cobra.Command {
	var (
		acquireLicense    bool
		dryRun            bool
		outputPath        string
		appID             int64
		bundleID          string
		externalVersionID string
		platformValue     string
	)

	cmd := &cobra.Command{
		Use:   "download",
		Short: "Download iOS, iPadOS, tvOS, watchOS, visionOS, and macOS app packages from the App Store",
		RunE: func(cmd *cobra.Command, args []string) error {
			if appID == 0 && bundleID == "" {
				return errors.New("either the app ID or the bundle identifier must be specified")
			}

			platform, err := appstore.ParsePlatform(platformValue)
			if err != nil {
				return err
			}

			store := appStore()
			infoResult, err := store.AccountInfo()
			if err != nil {
				return err
			}

			acc := infoResult.Account

			if dryRun {
				app, err := resolveDownloadApp(store, acc, appID, bundleID, platform)
				if err != nil {
					return err
				}

				destination, err := dryRunDestination(app, externalVersionID, outputPath, platform)
				if err != nil {
					return err
				}

				version := externalVersionID
				if version == "" {
					version = "latest"
				}

				displayedPlatform := platform
				if displayedPlatform == "" {
					displayedPlatform = appstore.PlatformIPhone
				}

				dependencies.Logger.Log().
					Bool("dry_run", true).
					Int64("app_id", app.ID).
					Str("bundle_id", app.BundleID).
					Str("platform", string(displayedPlatform)).
					Str("version", version).
					Str("output", destination).
					Bool("success", true).
					Msg("no download performed")

				return nil
			}

			var lastErr error
			purchaseRequired := false
			purchased := false

			return retry.Do(func() error {
				if errors.Is(lastErr, appstore.ErrPasswordTokenExpired) {
					loginResult, err := store.Login(appstore.LoginInput{
						Email:    acc.Email,
						Password: acc.Password,
					})
					if err != nil {
						return err
					}

					acc = loginResult.Account
				}

				app, err := resolveDownloadApp(store, acc, appID, bundleID, platform)
				if err != nil {
					return err
				}

				if errors.Is(lastErr, appstore.ErrLicenseRequired) {
					purchaseRequired = true
				}

				if purchaseRequired {
					err := store.Purchase(appstore.PurchaseInput{
						Account:  acc,
						App:      app,
						Platform: platform,
					})
					if err != nil && !errors.Is(err, appstore.ErrLicenseAlreadyExists) {
						return err
					}
					purchaseRequired = false
					purchased = true
					dependencies.Logger.Verbose().
						Bool("success", true).
						Msg("purchase")
				}

				interactive, _ := cmd.Context().Value(interactiveKey).(bool)
				var progress *progressbar.ProgressBar
				if interactive {
					progress = newDownloadProgressBar(os.Stdout)
				}

				out, err := store.Download(appstore.DownloadInput{
					Context:           cmd.Context(),
					Account:           acc,
					App:               app,
					OutputPath:        outputPath,
					Progress:          progress,
					ExternalVersionID: externalVersionID,
					Platform:          platform,
				})
				if err != nil {
					return err
				}

				if err := replicateDownloadSinf(store, platform, out); err != nil {
					return err
				}

				dependencies.Logger.Log().
					Str("output", out.DestinationPath).
					Bool("purchased", purchased).
					Bool("success", true).
					Send()

				return nil
			},
				retry.LastErrorOnly(true),
				retry.DelayType(retry.FixedDelay),
				retry.Delay(time.Millisecond),
				retry.Attempts(3),
				retry.RetryIf(func(err error) bool {
					lastErr = err

					if errors.Is(err, appstore.ErrPasswordTokenExpired) {
						return true
					}

					if errors.Is(err, appstore.ErrLicenseRequired) && acquireLicense {
						return true
					}

					return false
				}),
			)
		},
	}

	cmd.Flags().Int64VarP(&appID, "app-id", "i", 0, "ID of the target app (required)")
	cmd.Flags().StringVarP(&bundleID, "bundle-identifier", "b", "", "The bundle identifier of the target app (overrides the app ID when found)")
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "The destination path of the downloaded app package")
	cmd.Flags().StringVar(&externalVersionID, "external-version-id", "", "External version identifier of the target app (defaults to latest version when not specified)")
	cmd.Flags().StringVar(&platformValue, "platform", "", "Platform to download for: iphone (iOS), ipad (iPadOS), appletv (tvOS), watchos, visionos, or macos")
	cmd.Flags().BoolVar(&acquireLicense, "purchase", false, "Obtain a license for the app if needed")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Resolve the app, version, and output path without purchasing or downloading")

	return cmd
}

func resolveDownloadApp(store appstore.AppStore, acc appstore.Account, appID int64, bundleID string, platform appstore.Platform) (appstore.App, error) {
	app := appstore.App{ID: appID, BundleID: bundleID}
	if bundleID == "" {
		return app, nil
	}

	lookupResult, err := store.Lookup(appstore.LookupInput{
		Account:  acc,
		BundleID: bundleID,
		Platform: platform,
	})
	if err != nil && (appID == 0 || !errors.Is(err, appstore.ErrAppNotFound)) {
		return appstore.App{}, err
	}

	// Delisted apps may still be downloadable by their numeric ID.
	if err == nil {
		app = lookupResult.App
	}

	return app, nil
}

func dryRunDestination(app appstore.App, externalVersionID, outputPath string, platform appstore.Platform) (string, error) {
	version := externalVersionID
	if version == "" {
		version = "latest"
	}

	fileName := downloadPackageFileName(app, version, platform)
	if outputPath == "" {
		workdir, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("failed to get current directory: %w", err)
		}

		return filepath.Join(workdir, fileName), nil
	}

	info, err := os.Stat(outputPath)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("failed to determine whether path is a directory: %w", err)
	}

	if err == nil && info.IsDir() {
		return filepath.Join(outputPath, fileName), nil
	}

	return outputPath, nil
}

func downloadPackageFileName(app appstore.App, version string, platform appstore.Platform) string {
	var parts []string

	if app.BundleID != "" {
		parts = append(parts, app.BundleID)
	}

	if app.ID != 0 {
		parts = append(parts, strconv.FormatInt(app.ID, 10))
	}

	if version != "" {
		parts = append(parts, version)
	}

	extension := "ipa"
	if platform == appstore.PlatformMacOS {
		extension = "pkg"
	}

	return strings.Join(parts, "_") + "." + extension
}

// newDownloadProgressBar renders a single-line ASCII bar.
// Full-width Unicode blocks wrap in macOS terminals, and the following
// carriage return then paints the next update on a blank line.
func newDownloadProgressBar(writer io.Writer) *progressbar.ProgressBar {
	if writer == nil {
		writer = os.Stdout
	}

	return progressbar.NewOptions64(1,
		progressbar.OptionSetDescription("downloading"),
		progressbar.OptionSetWriter(writer),
		progressbar.OptionShowBytes(true),
		progressbar.OptionShowTotalBytes(true),
		progressbar.OptionSetWidth(40),
		progressbar.OptionThrottle(65*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionSetTheme(progressbar.ThemeASCII),
		progressbar.OptionUseANSICodes(true),
		progressbar.OptionSpinnerType(9),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionSetElapsedTime(false),
		progressbar.OptionSetPredictTime(false),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprint(writer, "\n")
		}),
	)
}

type sinfReplicator interface {
	ReplicateSinf(input appstore.ReplicateSinfInput) error
}

//nolint:wrapcheck
func replicateDownloadSinf(store sinfReplicator, platform appstore.Platform, out appstore.DownloadOutput) error {
	if platform == appstore.PlatformMacOS && len(out.Sinfs) == 0 {
		return nil
	}

	return store.ReplicateSinf(appstore.ReplicateSinfInput{Sinfs: out.Sinfs, PackagePath: out.DestinationPath})
}
