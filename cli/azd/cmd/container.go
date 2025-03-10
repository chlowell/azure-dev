package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resourcegraph/armresourcegraph"
	"github.com/MakeNowJust/heredoc/v2"
	"github.com/azure/azure-dev/cli/azd/cmd/actions"
	"github.com/azure/azure-dev/cli/azd/internal"
	"github.com/azure/azure-dev/cli/azd/internal/repository"
	"github.com/azure/azure-dev/cli/azd/pkg/account"
	"github.com/azure/azure-dev/cli/azd/pkg/alpha"
	"github.com/azure/azure-dev/cli/azd/pkg/auth"
	"github.com/azure/azure-dev/cli/azd/pkg/azapi"
	"github.com/azure/azure-dev/cli/azd/pkg/azd"
	"github.com/azure/azure-dev/cli/azd/pkg/azsdk"
	"github.com/azure/azure-dev/cli/azd/pkg/config"
	"github.com/azure/azure-dev/cli/azd/pkg/containerapps"
	"github.com/azure/azure-dev/cli/azd/pkg/devcenter"
	"github.com/azure/azure-dev/cli/azd/pkg/environment"
	"github.com/azure/azure-dev/cli/azd/pkg/environment/azdcontext"
	"github.com/azure/azure-dev/cli/azd/pkg/exec"
	"github.com/azure/azure-dev/cli/azd/pkg/httputil"
	"github.com/azure/azure-dev/cli/azd/pkg/infra"
	"github.com/azure/azure-dev/cli/azd/pkg/infra/provisioning"
	"github.com/azure/azure-dev/cli/azd/pkg/input"
	"github.com/azure/azure-dev/cli/azd/pkg/ioc"
	"github.com/azure/azure-dev/cli/azd/pkg/lazy"
	"github.com/azure/azure-dev/cli/azd/pkg/output"
	"github.com/azure/azure-dev/cli/azd/pkg/pipeline"
	"github.com/azure/azure-dev/cli/azd/pkg/platform"
	"github.com/azure/azure-dev/cli/azd/pkg/project"
	"github.com/azure/azure-dev/cli/azd/pkg/prompt"
	"github.com/azure/azure-dev/cli/azd/pkg/state"
	"github.com/azure/azure-dev/cli/azd/pkg/templates"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/azcli"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/docker"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/dotnet"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/git"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/github"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/javac"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/kubectl"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/maven"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/npm"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/python"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/swa"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/exp/slices"
)

// Registers a singleton action initializer for the specified action name
// This returns a function that when called resolves the action
// This is to ensure pre-conditions are met for composite actions like 'up'
// This finds the action for a named instance and casts it to the correct type for injection
func registerAction[T actions.Action](container *ioc.NestedContainer, actionName string) {
	container.RegisterSingleton(func() (T, error) {
		return resolveAction[T](container, actionName)
	})
}

// Registers a singleton action for the specified action name
// This finds the action for a named instance and casts it to the correct type for injection
func registerActionInitializer[T actions.Action](container *ioc.NestedContainer, actionName string) {
	container.RegisterSingleton(func() actions.ActionInitializer[T] {
		return func() (T, error) {
			return resolveAction[T](container, actionName)
		}
	})
}

// Resolves the action instance for the specified action name
// This finds the action for a named instance and casts it to the correct type for injection
func resolveAction[T actions.Action](container *ioc.NestedContainer, actionName string) (T, error) {
	var zero T
	var action actions.Action
	err := container.ResolveNamed(actionName, &action)
	if err != nil {
		return zero, err
	}

	instance, ok := action.(T)
	if !ok {
		return zero, fmt.Errorf("failed converting action to '%T'", zero)
	}

	return instance, nil
}

// Registers common Azd dependencies
func registerCommonDependencies(container *ioc.NestedContainer) {
	container.RegisterSingleton(output.GetCommandFormatter)

	container.RegisterSingleton(func(
		rootOptions *internal.GlobalCommandOptions,
		formatter output.Formatter,
		cmd *cobra.Command) input.Console {
		writer := cmd.OutOrStdout()
		// When using JSON formatting, we want to ensure we always write messages from the console to stderr.
		if formatter != nil && formatter.Kind() == output.JsonFormat {
			writer = cmd.ErrOrStderr()
		}

		if os.Getenv("NO_COLOR") != "" {
			writer = colorable.NewNonColorable(writer)
		}

		isTerminal := cmd.OutOrStdout() == os.Stdout &&
			cmd.InOrStdin() == os.Stdin && isatty.IsTerminal(os.Stdin.Fd()) &&
			isatty.IsTerminal(os.Stdout.Fd())

		return input.NewConsole(rootOptions.NoPrompt, isTerminal, writer, input.ConsoleHandles{
			Stdin:  cmd.InOrStdin(),
			Stdout: cmd.OutOrStdout(),
			Stderr: cmd.ErrOrStderr(),
		}, formatter)
	})

	container.RegisterSingleton(func(console input.Console, rootOptions *internal.GlobalCommandOptions) exec.CommandRunner {
		return exec.NewCommandRunner(
			&exec.RunnerOptions{
				Stdin:        console.Handles().Stdin,
				Stdout:       console.Handles().Stdout,
				Stderr:       console.Handles().Stderr,
				DebugLogging: rootOptions.EnableDebugLogging,
			})
	})

	client := createHttpClient()
	container.RegisterSingleton(func() httputil.HttpClient { return client })
	container.RegisterSingleton(func() auth.HttpClient { return client })
	container.RegisterSingleton(func() httputil.UserAgent {
		return httputil.UserAgent(internal.UserAgent())
	})

	// Auth
	container.RegisterSingleton(auth.NewLoggedInGuard)
	container.RegisterSingleton(auth.NewMultiTenantCredentialProvider)
	container.RegisterSingleton(func(mgr *auth.Manager) CredentialProviderFn {
		return mgr.CredentialForCurrentUser
	})

	container.RegisterSingleton(func(console input.Console) io.Writer {
		writer := console.Handles().Stdout

		if os.Getenv("NO_COLOR") != "" {
			writer = colorable.NewNonColorable(writer)
		}

		return writer
	})

	container.RegisterSingleton(func(cmd *cobra.Command) envFlag {
		envValue, err := cmd.Flags().GetString(environmentNameFlag)
		if err != nil {
			panic("command asked for envFlag, but envFlag was not included in cmd.Flags().")
		}

		return envFlag{environmentName: envValue}
	})

	container.RegisterSingleton(func(cmd *cobra.Command) CmdAnnotations {
		return cmd.Annotations
	})

	// Azd Context
	container.RegisterSingleton(azdcontext.NewAzdContext)

	// Lazy loads the Azd context after the azure.yaml file becomes available
	container.RegisterSingleton(func() *lazy.Lazy[*azdcontext.AzdContext] {
		return lazy.NewLazy(func() (*azdcontext.AzdContext, error) {
			return azdcontext.NewAzdContext()
		})
	})

	// Register an initialized environment based on the specified environment flag, or the default environment.
	// Note that referencing an *environment.Environment in a command automatically triggers a UI prompt if the
	// environment is uninitialized or a default environment doesn't yet exist.
	container.RegisterSingleton(
		func(ctx context.Context,
			azdContext *azdcontext.AzdContext,
			envManager environment.Manager,
			lazyEnv *lazy.Lazy[*environment.Environment],
			envFlags envFlag,
		) (*environment.Environment, error) {
			if azdContext == nil {
				return nil, azdcontext.ErrNoProject
			}

			environmentName := envFlags.environmentName
			var err error

			env, err := envManager.LoadOrCreateInteractive(ctx, environmentName)
			if err != nil {
				return nil, fmt.Errorf("loading environment: %w", err)
			}

			// Reset lazy env value after loading or creating environment
			// This allows any previous lazy instances (such as hooks) to now point to the same instance
			lazyEnv.SetValue(env)

			return env, nil
		},
	)
	container.RegisterSingleton(func(lazyEnvManager *lazy.Lazy[environment.Manager]) environment.EnvironmentResolver {
		return func(ctx context.Context) (*environment.Environment, error) {
			azdCtx, err := azdcontext.NewAzdContext()
			if err != nil {
				return nil, err
			}
			defaultEnv, err := azdCtx.GetDefaultEnvironmentName()
			if err != nil {
				return nil, err
			}

			// We need to lazy load the environment manager since it depends on azd context
			envManager, err := lazyEnvManager.GetValue()
			if err != nil {
				return nil, err
			}

			return envManager.Get(ctx, defaultEnv)
		}
	})

	container.RegisterSingleton(environment.NewLocalFileDataStore)
	container.RegisterSingleton(environment.NewManager)

	container.RegisterSingleton(func() *lazy.Lazy[environment.LocalDataStore] {
		return lazy.NewLazy(func() (environment.LocalDataStore, error) {
			var localDataStore environment.LocalDataStore
			err := container.Resolve(&localDataStore)
			if err != nil {
				return nil, err
			}

			return localDataStore, nil
		})
	})

	// Environment manager depends on azd context
	container.RegisterSingleton(func(azdContext *lazy.Lazy[*azdcontext.AzdContext]) *lazy.Lazy[environment.Manager] {
		return lazy.NewLazy(func() (environment.Manager, error) {
			azdCtx, err := azdContext.GetValue()
			if err != nil {
				return nil, err
			}

			// Register the Azd context instance as a singleton in the container if now available
			ioc.RegisterInstance(container, azdCtx)

			var envManager environment.Manager
			err = container.Resolve(&envManager)
			if err != nil {
				return nil, err
			}

			return envManager, nil
		})
	})

	container.RegisterSingleton(func(
		lazyProjectConfig *lazy.Lazy[*project.ProjectConfig],
		userConfigManager config.UserConfigManager,
	) (*state.RemoteConfig, error) {
		var remoteStateConfig *state.RemoteConfig

		userConfig, err := userConfigManager.Load()
		if err != nil {
			return nil, fmt.Errorf("loading user config: %w", err)
		}

		// The project config may not be available yet
		// Ex) Within init phase of fingerprinting
		projectConfig, _ := lazyProjectConfig.GetValue()

		// Lookup remote state config in the following precedence:
		// 1. Project azure.yaml
		// 2. User configuration
		if projectConfig != nil && projectConfig.State != nil && projectConfig.State.Remote != nil {
			remoteStateConfig = projectConfig.State.Remote
		} else {
			if _, err := userConfig.GetSection("state.remote", &remoteStateConfig); err != nil {
				return nil, fmt.Errorf("getting remote state config: %w", err)
			}
		}

		return remoteStateConfig, nil
	})

	// Lazy loads an existing environment, erroring out if not available
	// One can repeatedly call GetValue to wait until the environment is available.
	container.RegisterSingleton(
		func(
			ctx context.Context,
			lazyEnvManager *lazy.Lazy[environment.Manager],
			lazyAzdContext *lazy.Lazy[*azdcontext.AzdContext],
			envFlags envFlag,
		) *lazy.Lazy[*environment.Environment] {
			return lazy.NewLazy(func() (*environment.Environment, error) {
				azdCtx, err := lazyAzdContext.GetValue()
				if err != nil {
					return nil, err
				}

				environmentName := envFlags.environmentName
				if environmentName == "" {
					environmentName, err = azdCtx.GetDefaultEnvironmentName()
					if err != nil {
						return nil, err
					}
				}

				envManager, err := lazyEnvManager.GetValue()
				if err != nil {
					return nil, err
				}

				env, err := envManager.Get(ctx, environmentName)
				if err != nil {
					return nil, err
				}

				return env, err
			})
		},
	)

	// Project Config
	container.RegisterSingleton(
		func(ctx context.Context, azdContext *azdcontext.AzdContext) (*project.ProjectConfig, error) {
			if azdContext == nil {
				return nil, azdcontext.ErrNoProject
			}

			projectConfig, err := project.Load(ctx, azdContext.ProjectPath())
			if err != nil {
				return nil, err
			}

			return projectConfig, nil
		},
	)

	// Lazy loads the project config from the Azd Context when it becomes available
	container.RegisterSingleton(func(lazyAzdContext *lazy.Lazy[*azdcontext.AzdContext]) *lazy.Lazy[*project.ProjectConfig] {
		return lazy.NewLazy(func() (*project.ProjectConfig, error) {
			_, err := lazyAzdContext.GetValue()
			if err != nil {
				return nil, err
			}

			var projectConfig *project.ProjectConfig
			err = container.Resolve(&projectConfig)

			return projectConfig, err
		})
	})

	container.RegisterSingleton(func(
		ctx context.Context,
		credential azcore.TokenCredential,
		httpClient httputil.HttpClient,
	) (*armresourcegraph.Client, error) {
		options := azsdk.
			DefaultClientOptionsBuilder(ctx, httpClient, "azd").
			BuildArmClientOptions()

		return armresourcegraph.NewClient(credential, options)
	})

	container.RegisterSingleton(templates.NewTemplateManager)
	container.RegisterSingleton(templates.NewSourceManager)
	container.RegisterSingleton(project.NewResourceManager)
	container.RegisterSingleton(func() *lazy.Lazy[project.ResourceManager] {
		return lazy.NewLazy(func() (project.ResourceManager, error) {
			var resourceManager project.ResourceManager
			err := container.Resolve(&resourceManager)

			return resourceManager, err
		})
	})
	container.RegisterSingleton(project.NewProjectManager)
	container.RegisterSingleton(project.NewDotNetImporter)
	container.RegisterSingleton(project.NewImportManager)
	container.RegisterSingleton(project.NewServiceManager)
	container.RegisterSingleton(func() *lazy.Lazy[project.ServiceManager] {
		return lazy.NewLazy(func() (project.ServiceManager, error) {
			var serviceManager project.ServiceManager
			err := container.Resolve(&serviceManager)

			return serviceManager, err
		})
	})
	container.RegisterSingleton(repository.NewInitializer)
	container.RegisterSingleton(alpha.NewFeaturesManager)
	container.RegisterSingleton(config.NewUserConfigManager)
	container.RegisterSingleton(config.NewManager)
	container.RegisterSingleton(config.NewFileConfigManager)
	container.RegisterSingleton(auth.NewManager)
	container.RegisterSingleton(azcli.NewUserProfileService)
	container.RegisterSingleton(account.NewSubscriptionsService)
	container.RegisterSingleton(account.NewManager)
	container.RegisterSingleton(account.NewSubscriptionsManager)
	container.RegisterSingleton(account.NewSubscriptionCredentialProvider)
	container.RegisterSingleton(azcli.NewManagedClustersService)
	container.RegisterSingleton(azcli.NewAdService)
	container.RegisterSingleton(azcli.NewContainerRegistryService)
	container.RegisterSingleton(containerapps.NewContainerAppService)
	container.RegisterSingleton(project.NewContainerHelper)
	container.RegisterSingleton(azcli.NewSpringService)
	container.RegisterSingleton(func() ioc.ServiceLocator {
		return ioc.NewServiceLocator(container)
	})

	container.RegisterSingleton(func(subManager *account.SubscriptionsManager) account.SubscriptionTenantResolver {
		return subManager
	})

	container.RegisterSingleton(func(ctx context.Context, authManager *auth.Manager) (azcore.TokenCredential, error) {
		return authManager.CredentialForCurrentUser(ctx, nil)
	})

	// Tools
	container.RegisterSingleton(func(
		rootOptions *internal.GlobalCommandOptions,
		credentialProvider account.SubscriptionCredentialProvider,
		httpClient httputil.HttpClient,
	) azcli.AzCli {
		return azcli.NewAzCli(credentialProvider, httpClient, azcli.NewAzCliArgs{
			EnableDebug:     rootOptions.EnableDebugLogging,
			EnableTelemetry: rootOptions.EnableTelemetry,
		})
	})
	container.RegisterSingleton(azapi.NewDeployments)
	container.RegisterSingleton(azapi.NewDeploymentOperations)
	container.RegisterSingleton(docker.NewDocker)
	container.RegisterSingleton(dotnet.NewDotNetCli)
	container.RegisterSingleton(git.NewGitCli)
	container.RegisterSingleton(github.NewGitHubCli)
	container.RegisterSingleton(javac.NewCli)
	container.RegisterSingleton(kubectl.NewKubectl)
	container.RegisterSingleton(maven.NewMavenCli)
	container.RegisterSingleton(npm.NewNpmCli)
	container.RegisterSingleton(python.NewPythonCli)
	container.RegisterSingleton(swa.NewSwaCli)

	// Provisioning
	container.RegisterSingleton(infra.NewAzureResourceManager)
	container.RegisterTransient(provisioning.NewManager)
	container.RegisterSingleton(provisioning.NewPrincipalIdProvider)
	container.RegisterSingleton(prompt.NewDefaultPrompter)

	// Other
	container.RegisterSingleton(createClock)

	// Service Targets
	serviceTargetMap := map[project.ServiceTargetKind]any{
		"":                               project.NewAppServiceTarget,
		project.AppServiceTarget:         project.NewAppServiceTarget,
		project.AzureFunctionTarget:      project.NewFunctionAppTarget,
		project.ContainerAppTarget:       project.NewContainerAppTarget,
		project.StaticWebAppTarget:       project.NewStaticWebAppTarget,
		project.AksTarget:                project.NewAksTarget,
		project.SpringAppTarget:          project.NewSpringAppTarget,
		project.DotNetContainerAppTarget: project.NewDotNetContainerAppTarget,
	}

	for target, constructor := range serviceTargetMap {
		if err := container.RegisterNamedSingleton(string(target), constructor); err != nil {
			panic(fmt.Errorf("registering service target %s: %w", target, err))
		}
	}

	// Languages
	frameworkServiceMap := map[project.ServiceLanguageKind]any{
		"":                                project.NewDotNetProject,
		project.ServiceLanguageDotNet:     project.NewDotNetProject,
		project.ServiceLanguageCsharp:     project.NewDotNetProject,
		project.ServiceLanguageFsharp:     project.NewDotNetProject,
		project.ServiceLanguagePython:     project.NewPythonProject,
		project.ServiceLanguageJavaScript: project.NewNpmProject,
		project.ServiceLanguageTypeScript: project.NewNpmProject,
		project.ServiceLanguageJava:       project.NewMavenProject,
		project.ServiceLanguageDocker:     project.NewDockerProject,
	}

	for language, constructor := range frameworkServiceMap {
		if err := container.RegisterNamedSingleton(string(language), constructor); err != nil {
			panic(fmt.Errorf("registering framework service %s: %w", language, err))
		}
	}

	// Pipelines
	container.RegisterSingleton(pipeline.NewPipelineManager)
	container.RegisterSingleton(func(flags *pipelineConfigFlags) *pipeline.PipelineManagerArgs {
		return &flags.PipelineManagerArgs
	})

	pipelineProviderMap := map[string]any{
		"github-ci":  pipeline.NewGitHubCiProvider,
		"github-scm": pipeline.NewGitHubScmProvider,
		"azdo-ci":    pipeline.NewAzdoCiProvider,
		"azdo-scm":   pipeline.NewAzdoScmProvider,
	}

	for provider, constructor := range pipelineProviderMap {
		if err := container.RegisterNamedSingleton(string(provider), constructor); err != nil {
			panic(fmt.Errorf("registering pipeline provider %s: %w", provider, err))
		}
	}

	// Platform configuration
	container.RegisterSingleton(func() *lazy.Lazy[*platform.Config] {
		return lazy.NewLazy(func() (*platform.Config, error) {
			var platformConfig *platform.Config
			err := container.Resolve(&platformConfig)

			return platformConfig, err
		})
	})

	container.RegisterSingleton(func(
		lazyProjectConfig *lazy.Lazy[*project.ProjectConfig],
		userConfigManager config.UserConfigManager,
	) (*platform.Config, error) {
		// First check `azure.yaml` for platform configuration section
		projectConfig, err := lazyProjectConfig.GetValue()
		if err == nil && projectConfig != nil && projectConfig.Platform != nil {
			return projectConfig.Platform, nil
		}

		// Fallback to global user configuration
		config, err := userConfigManager.Load()
		if err != nil {
			return nil, fmt.Errorf("loading user config: %w", err)
		}

		var platformConfig *platform.Config
		ok, err := config.GetSection("platform", &platformConfig)
		if err != nil {
			return nil, fmt.Errorf("getting platform config: %w", err)
		}

		if !ok || platformConfig.Type == "" {
			return nil, platform.ErrPlatformConfigNotFound
		}

		// Validate platform type
		supportedPlatformKinds := []string{
			string(devcenter.PlatformKindDevCenter),
			string(azd.PlatformKindDefault),
		}
		if !slices.Contains(supportedPlatformKinds, string(platformConfig.Type)) {
			return nil, fmt.Errorf(
				heredoc.Doc(`platform type '%s' is not supported. Valid values are '%s'.
				Run %s to set or %s to reset. (%w)`),
				platformConfig.Type,
				strings.Join(supportedPlatformKinds, ","),
				output.WithBackticks("azd config set platform.type <type>"),
				output.WithBackticks("azd config unset platform.type"),
				platform.ErrPlatformNotSupported,
			)
		}

		return platformConfig, nil
	})

	// Platform Providers
	platformProviderMap := map[platform.PlatformKind]any{
		azd.PlatformKindDefault:         azd.NewDefaultPlatform,
		devcenter.PlatformKindDevCenter: devcenter.NewPlatform,
	}

	for provider, constructor := range platformProviderMap {
		platformName := fmt.Sprintf("%s-platform", provider)
		if err := container.RegisterNamedSingleton(platformName, constructor); err != nil {
			panic(fmt.Errorf("registering platform provider %s: %w", provider, err))
		}
	}

	// Required for nested actions called from composite actions like 'up'
	registerActionInitializer[*initAction](container, "azd-init-action")
	registerActionInitializer[*provisionAction](container, "azd-provision-action")
	registerActionInitializer[*restoreAction](container, "azd-restore-action")
	registerActionInitializer[*buildAction](container, "azd-build-action")
	registerActionInitializer[*packageAction](container, "azd-package-action")
	registerActionInitializer[*deployAction](container, "azd-deploy-action")

	registerAction[*provisionAction](container, "azd-provision-action")
	registerAction[*downAction](container, "azd-down-action")
	registerAction[*configShowAction](container, "azd-config-show-action")
}
