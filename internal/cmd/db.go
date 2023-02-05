package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/athoscouto/codename"
	"github.com/briandowns/spinner"
	"github.com/chiselstrike/iku-turso-cli/internal/settings"
	"github.com/chiselstrike/iku-turso-cli/internal/turso"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// Color function for emphasising text.
var emph = color.New(color.FgBlue, color.Bold).SprintFunc()

var warn = color.New(color.FgYellow, color.Bold).SprintFunc()

var canary bool
var showUrlFlag bool
var region string
var yesFlag bool
var instanceFlag string
var regionFlag string

func getRegionIds(client *turso.Client) []string {
	regions, err := turso.GetRegions(client)
	if err != nil {
		return []string{}
	}
	return regions.Ids
}

func extractDatabaseNames(databases []turso.Database) []string {
	names := make([]string, 0)
	for _, database := range databases {
		name := database.Name
		ty := database.Type
		if ty == "primary" {
			names = append(names, name)
		}
	}
	return names
}

func fetchDatabaseNames(client *turso.Client) []string {
	databases, err := getDatabases(client)
	if err != nil {
		return []string{}
	}
	return extractDatabaseNames(databases)
}

func getDatabase(client *turso.Client, name string) (turso.Database, error) {
	databases, err := getDatabases(client)
	if err != nil {
		return turso.Database{}, err
	}

	for _, database := range databases {
		if database.Name == name {
			return database, nil
		}
	}

	return turso.Database{}, fmt.Errorf("database with name %s not found", name)
}

func getDatabaseNames(client *turso.Client) []string {
	settings, err := settings.ReadSettings()
	if err != nil {
		return fetchDatabaseNames(client)
	}
	cached_names := settings.GetDbNamesCache()
	if cached_names != nil {
		return cached_names
	}
	names := fetchDatabaseNames(client)
	settings.SetDbNamesCache(names)
	return names
}

func getDatabases(client *turso.Client) ([]turso.Database, error) {
	return client.Databases.List()
}

func init() {
	rootCmd.AddCommand(dbCmd)
	dbCmd.AddCommand(createCmd, shellCmd, destroyCmd, replicateCmd, listCmd, regionsCmd, showCmd)
	destroyCmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "Confirms the destruction of all regions of the database.")
	destroyCmd.Flags().StringVar(&regionFlag, "region", "", "Pick a database region to destroy.")
	destroyCmd.Flags().StringVar(&instanceFlag, "instance", "", "Pick a specific database instance to destroy.")
	createCmd.Flags().BoolVar(&canary, "canary", false, "Use database canary build.")
	createCmd.Flags().StringVar(&region, "region", "", "Region ID. If no ID is specified, closest region to you is used by default.")
	createCmd.RegisterFlagCompletionFunc("region", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return getRegionIds(createTursoClient()), cobra.ShellCompDirectiveDefault
	})
	replicateCmd.Flags().BoolVar(&canary, "canary", false, "Use database canary build.")
	showCmd.Flags().BoolVar(&showUrlFlag, "url", false, "Show database connection URL.")
}

var dbCmd = &cobra.Command{
	Use:   "db",
	Short: "Manage databases",
}

func getAccessToken() (string, error) {
	settings, err := settings.ReadSettings()
	if err != nil {
		return "", fmt.Errorf("could not read local settings")
	}

	token := settings.GetToken()
	if token == "" {
		return "", fmt.Errorf("user not logged in")
	}

	return token, nil
}

func getHost() string {
	host := os.Getenv("TURSO_API_BASEURL")
	if host == "" {
		host = "https://api.chiseledge.com"
	}
	return host
}

var createCmd = &cobra.Command{
	Use:               "create [flags] [database_name]",
	Short:             "Create a database.",
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: noFilesArg,
	RunE: func(cmd *cobra.Command, args []string) error {
		config, err := settings.ReadSettings()
		if err != nil {
			return err
		}
		name := ""
		if len(args) == 0 || args[0] == "" {
			rng, err := codename.DefaultRNG()
			if err != nil {
				return err
			}
			name = codename.Generate(rng, 0)
		} else {
			name = args[0]
		}
		client := createTursoClient()
		region := region
		if region != "" && !isValidRegion(client, region) {
			return fmt.Errorf("region '%s' is not a valid one", region)
		}
		if region == "" {
			region = probeClosestRegion(client)
		}
		var image string
		if canary {
			image = "canary"
		} else {
			image = "latest"
		}
		start := time.Now()
		regionText := fmt.Sprintf("%s (%s)", toLocation(region), region)
		description := fmt.Sprintf("Creating database %s in %s ", emph(name), emph(regionText))
		bar := startLoadingBar(description)
		defer bar.Stop()
		res, err := client.Databases.Create(name, region, image)
		if err != nil {
			return fmt.Errorf("could not create database %s: %w", name, err)
		}
		dbSettings := settings.DatabaseSettings{
			Name:     res.Database.Name,
			Host:     res.Database.Hostname,
			Username: res.Username,
			Password: res.Password,
		}

		if _, err = client.Instances.Create(name, res.Password, region, image); err != nil {
			return fmt.Errorf("failed to create instance for database %s: %w", name, err)
		}

		bar.Stop()
		elapsed := time.Since(start)
		fmt.Printf("Created database %s to %s in %d seconds.\n\n", emph(name), emph(regionText), int(elapsed.Seconds()))

		fmt.Printf("You can start an interactive SQL shell with:\n\n")
		fmt.Printf("   turso db shell %s\n\n", name)
		fmt.Printf("To obtain connection URL, run:\n\n")
		fmt.Printf("   turso db show --url %s\n\n", name)
		config.AddDatabase(res.Database.ID, &dbSettings)
		config.InvalidateDbNamesCache()
		return nil
	},
}

// The fallback region ID to use if we are unable to probe the closest region.
const FallbackRegionId = "ams"

const FallbackWarning = "Warning: we could not determine the deployment region closest to your physical location.\nThe region is defaulting to Amsterdam (ams). Consider specifying a region to select a better option using\n\n\tturso db create --region [region].\n\nRun turso db regions for a list of supported regions.\n"

type Region struct {
	Server string
}

func probeClosestRegion(client *turso.Client) string {
	probeUrl := "https://chisel-region.fly.dev"
	resp, err := http.Get(probeUrl)
	if err != nil {
		fmt.Printf(warn(FallbackWarning))
		return FallbackRegionId
	}
	defer resp.Body.Close()

	reg := Region{}
	err = json.NewDecoder(resp.Body).Decode(&reg)
	if err != nil {
		return FallbackRegionId
	}

	// Fly has regions that are not available to users. So let's ensure
	// that we return a region ID that is actually usable for provisioning
	// a database.
	if isValidRegion(client, reg.Server) {
		return reg.Server
	}
	return FallbackRegionId
}

func isValidRegion(client *turso.Client, region string) bool {
	regionIds := getRegionIds(client)
	if len(regionIds) == 0 {
		return true
	}
	for _, regionId := range regionIds {
		if region == regionId {
			return true
		}
	}
	return false
}
func destroyArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return getDatabaseNames(createTursoClient()), cobra.ShellCompDirectiveNoFileComp
	}
	return []string{}, cobra.ShellCompDirectiveNoFileComp
}

var destroyCmd = &cobra.Command{
	Use:               "destroy database_name",
	Short:             "Destroy a database.",
	Args:              cobra.MatchAll(cobra.ExactArgs(1), dbNameValidator(0)),
	ValidArgsFunction: destroyArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		client := createTursoClient()
		name := args[0]
		if instanceFlag != "" {
			return destroyDatabaseInstance(client, name, instanceFlag)
		}

		if regionFlag != "" {
			return destroyDatabaseRegion(client, name, regionFlag)
		}

		if yesFlag {
			return destroyDatabase(client, name)
		}

		fmt.Printf("Database %s, all its replicas, and data will be destroyed.\n", emph(name))

		ok, err := promptConfirmation("Are you sure you want to do this?")
		if err != nil {
			return fmt.Errorf("could not get prompt confirmed by user: %w", err)
		}

		if !ok {
			fmt.Println("Database destruction avoided.")
			return nil
		}

		return destroyDatabase(client, name)
	},
}

var showCmd = &cobra.Command{
	Use:   "show database_name",
	Short: "Show information from a database.",
	Args: cobra.MatchAll(
		cobra.ExactArgs(1),
		dbNameValidator(0),
	),
	RunE: func(cmd *cobra.Command, args []string) error {
		client := createTursoClient()
		db, err := getDatabase(client, args[0])
		if err != nil {
			return err
		}

		if db.Type != "logical" {
			return fmt.Errorf("only new databases, of type 'logical', support the show operation")
		}

		config, err := settings.ReadSettings()
		if err != nil {
			return err
		}

		if showUrlFlag {
			fmt.Println(getDatabaseUrl(config, db))
			return nil
		}

		instances, err := client.Instances.List(db.Name)
		if err != nil {
			return fmt.Errorf("could not get instances of database %s: %w", db.Name, err)
		}

		regions := make([]string, len(db.Regions))
		copy(regions, db.Regions)
		sort.Strings(regions)

		fmt.Println("Name:    ", db.Name)
		fmt.Println("URL:     ", getDatabaseUrl(config, db))
		fmt.Println("ID:      ", db.ID)
		fmt.Println("Regions: ", strings.Join(regions, ", "))
		fmt.Println()

		data := [][]string{}
		for _, instance := range instances {
			url := getInstanceUrl(config, db, instance)
			data = append(data, []string{instance.Name, instance.Type, instance.Region, url})
		}

		fmt.Print("Database Instances:\n")
		printTable([]string{"name", "type", "region", "url"}, data)

		return nil
	},
}

func replicateArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 1 {
		return getRegionIds(createTursoClient()), cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
	if len(args) == 0 {
		return getDatabaseNames(createTursoClient()), cobra.ShellCompDirectiveNoFileComp
	}
	return []string{}, cobra.ShellCompDirectiveNoFileComp
}

var replicateCmd = &cobra.Command{
	Use:               "replicate database_name region_id",
	Short:             "Replicate a database.",
	Args:              cobra.ExactArgs(2),
	ValidArgsFunction: replicateArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		config, err := settings.ReadSettings()
		if err != nil {
			return err
		}
		name := args[0]
		if name == "" {
			return fmt.Errorf("You must specify a database name to replicate it.")
		}
		region := args[1]
		if region == "" {
			return fmt.Errorf("You must specify a database region ID to replicate it.")
		}
		tursoClient := createTursoClient()
		if !isValidRegion(tursoClient, region) {
			return fmt.Errorf("Invalid region ID. Run %s to see a list of valid region IDs.", emph("turso db regions"))
		}
		var image string
		if canary {
			image = "canary"
		} else {
			image = "latest"
		}
		accessToken, err := getAccessToken()
		if err != nil {
			return fmt.Errorf("please login with %s", emph("turso auth login"))
		}
		host := getHost()

		original, err := getDatabase(tursoClient, name)
		if err != nil {
			return fmt.Errorf("please login with %s", emph("turso auth login"))
		}

		url := fmt.Sprintf("%s/v1/databases", host)
		if original.Type == "logical" {
			url = fmt.Sprintf("%s/v2/databases/%s/instances", host, name)
		}

		bearer := "Bearer " + accessToken
		dbSettings := config.GetDatabaseSettings(original.ID)
		password := dbSettings.Password

		createDbReq := []byte(fmt.Sprintf(`{"name": "%s", "region": "%s", "image": "%s", "type": "replica", "password": "%s"}`, name, region, image, password))
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(createDbReq))
		if err != nil {
			return err
		}
		req.Header.Add("Authorization", bearer)
		s := spinner.New(spinner.CharSets[36], 800*time.Millisecond)
		regionText := fmt.Sprintf("%s (%s)", toLocation(region), region)
		s.Prefix = fmt.Sprintf("Replicating database %s to %s ", emph(name), emph(regionText))
		s.Start()
		start := time.Now()
		client := &http.Client{}
		resp, err := client.Do(req)
		s.Stop()
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("Failed to create database: %s", resp.Status)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		var result interface{}
		if err := json.Unmarshal(body, &result); err != nil {
			return err
		}
		end := time.Now()
		elapsed := end.Sub(start)
		var m map[string]interface{}
		if original.Type == "logical" {
			m = result.(map[string]interface{})["instance"].(map[string]interface{})
		} else {
			m = result.(map[string]interface{})["database"].(map[string]interface{})
		}
		username := result.(map[string]interface{})["username"].(string)
		password = result.(map[string]interface{})["password"].(string)
		var dbId, dbHost string
		if original.Type == "logical" {
			dbId = m["uuid"].(string)
			dbHost = original.Hostname
		} else {
			dbId = m["DbId"].(string)
			dbHost = m["Hostname"].(string)
		}
		fmt.Printf("Replicated database %s to %s in %d seconds.\n\n", emph(name), emph(regionText), int(elapsed.Seconds()))
		dbSettings = &settings.DatabaseSettings{
			Host:     dbHost,
			Username: username,
			Password: password,
		}
		fmt.Printf("HTTP connection string:\n\n")
		dbUrl := dbSettings.GetURL()
		fmt.Printf("   %s\n\n", dbUrl)
		fmt.Printf("You can start an interactive SQL shell with:\n\n")
		fmt.Printf("   turso db shell %s\n\n", dbUrl)
		config.AddDatabase(dbId, dbSettings)
		config.InvalidateDbNamesCache()
		return nil
	},
}

var listCmd = &cobra.Command{
	Use:               "list",
	Short:             "List databases.",
	Args:              cobra.NoArgs,
	ValidArgsFunction: noFilesArg,
	RunE: func(cmd *cobra.Command, args []string) error {
		settings, err := settings.ReadSettings()
		if err != nil {
			return err
		}
		databases, err := getDatabases(createTursoClient())
		if err != nil {
			return err
		}
		data := [][]string{}
		for _, database := range databases {
			url := getDatabaseUrl(settings, database)
			regions := getDatabaseRegions(database)
			data = append(data, []string{database.Name, database.Type, regions, url})
		}
		printTable([]string{"name", "type", "regions", "url"}, data)
		settings.SetDbNamesCache(extractDatabaseNames(databases))
		return nil
	},
}

var regionsCmd = &cobra.Command{
	Use:               "regions",
	Short:             "List available database regions.",
	Args:              cobra.NoArgs,
	ValidArgsFunction: noFilesArg,
	Run: func(cmd *cobra.Command, args []string) {
		client := createTursoClient()
		defaultRegionId := probeClosestRegion(client)
		fmt.Println("ID   LOCATION")
		for _, regionId := range getRegionIds(client) {
			suffix := ""
			if regionId == defaultRegionId {
				suffix = "  [default]"
			}
			line := fmt.Sprintf("%s  %s%s", regionId, toLocation(regionId), suffix)
			if regionId == defaultRegionId {
				line = emph(line)
			}
			fmt.Printf("%s\n", line)
		}
	},
}

func toLocation(regionId string) string {
	switch regionId {
	case "ams":
		return "Amsterdam, Netherlands"
	case "cdg":
		return "Paris, France"
	case "den":
		return "Denver, Colorado (US)"
	case "dfw":
		return "Dallas, Texas (US)"
	case "ewr":
		return "Secaucus, NJ (US)"
	case "fra":
		return "Frankfurt, Germany"
	case "gru":
		return "São Paulo, Brazil"
	case "hkg":
		return "Hong Kong, Hong Kong"
	case "iad":
		return "Ashburn, Virginia (US)"
	case "jnb":
		return "Johannesburg, South Africa"
	case "lax":
		return "Los Angeles, California (US)"
	case "lhr":
		return "London, United Kingdom"
	case "maa":
		return "Chennai (Madras), India"
	case "mad":
		return "Madrid, Spain"
	case "mia":
		return "Miami, Florida (US)"
	case "nrt":
		return "Tokyo, Japan"
	case "ord":
		return "Chicago, Illinois (US)"
	case "otp":
		return "Bucharest, Romania"
	case "scl":
		return "Santiago, Chile"
	case "sea":
		return "Seattle, Washington (US)"
	case "sin":
		return "Singapore"
	case "sjc":
		return "Sunnyvale, California (US)"
	case "syd":
		return "Sydney, Australia"
	case "waw":
		return "Warsaw, Poland"
	case "yul":
		return "Montreal, Canada"
	case "yyz":
		return "Toronto, Canada"
	default:
		return fmt.Sprintf("Region ID: %s", regionId)
	}
}
