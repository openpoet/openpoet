package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	operation := Operation(os.Args[1])
	if operation != OperationPrepare && operation != OperationPreflight && operation != OperationApply {
		usage()
		os.Exit(2)
	}

	config, err := parseConfig(operation, os.Args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(2)
	}
	plan, _ := BuildPlan(operation)
	printPlan(operation, config.Execute, plan)
	if !config.Execute {
		fmt.Println("\ndry-run concluído; nenhuma ação foi executada")
		return
	}

	workflow := NewWorkflow(os.Stdout)
	ctx := context.Background()
	switch operation {
	case OperationPrepare:
		manifest, manifestPath, err := workflow.Prepare(ctx, config)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("\nrelease preparada: %s\n", manifest.ReleaseID)
		fmt.Printf("manifest: %s\n", manifestPath)
		fmt.Printf("confirmation token: %s\n", manifest.ConfirmationToken)
		fmt.Println("o token ainda não autoriza deploy; apply também exige aprovação humana explícita")
	case OperationPreflight:
		manifest, err := workflow.Preflight(ctx, config)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("\npreflight OK: %s\n", manifest.ReleaseID)
	case OperationApply:
		result, err := workflow.Apply(ctx, config)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("\napply OK: %s\n", result.ReleaseID)
		fmt.Printf("backup SQLite: %s\n", result.BackupPath)
		fmt.Printf("binário anterior: %s\n", result.PreviousBinary)
	}
}

func parseConfig(operation Operation, args []string) (Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return Config{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}

	flags := flag.NewFlagSet(string(operation), flag.ContinueOnError)
	config := Config{
		Operation: operation, EncryptKey: os.Getenv("OPENPOET_ENCRYPT_KEY"), MigrateSecrets: true,
	}
	flags.BoolVar(&config.Execute, "execute", false, "executa o plano; sem esta flag é sempre dry-run")
	flags.StringVar(&config.ConfirmToken, "confirm-token", "", "token exato do manifest; obrigatório para apply real")
	flags.StringVar(&config.RepoDir, "repo", cwd, "raiz do repositório OpenPoet")
	flags.StringVar(
		&config.ReleasesDir,
		"releases-dir",
		filepath.Join(home, ".openpoet-rollout", "releases"),
		"diretório de releases imutáveis",
	)
	flags.StringVar(&config.ReleaseID, "release-id", "", "ID seguro da release; gerado no prepare se omitido")
	flags.StringVar(&config.ManifestPath, "manifest", "", "manifest.json gerado por prepare")
	flags.StringVar(&config.LiveBinary, "live-binary", filepath.Join(cwd, ".run", "openpoet"), "binário live")
	flags.StringVar(&config.DBPath, "db", filepath.Join(cwd, ".run", "openpoet.db"), "SQLite de produção")
	flags.StringVar(
		&config.BackupDir,
		"backup-dir",
		filepath.Join(home, ".openpoet-rollout", "backups"),
		"diretório de snapshots SQLite",
	)
	flags.StringVar(
		&config.PlistPath,
		"plist",
		filepath.Join(cwd, ".run", "openpoet.production.plist"),
		"plist do LaunchAgent",
	)
	flags.StringVar(&config.ServiceLabel, "service-label", "local.openpoet.production", "label launchd")
	flags.StringVar(&config.LaunchDomain, "launch-domain", fmt.Sprintf("gui/%d", os.Getuid()), "domínio launchd")
	flags.StringVar(&config.HealthURL, "health-url", "http://127.0.0.1:8081/api/version", "health/version endpoint")
	flags.StringVar(&config.RelayURL, "relay-url", "", "DefaultRelayURL embutida no build")
	flags.DurationVar(&config.StopTimeout, "stop-timeout", 30*time.Second, "limite para graceful stop")
	flags.DurationVar(&config.HealthTimeout, "health-timeout", 45*time.Second, "limite para health gates")
	flags.DurationVar(&config.HealthInterval, "health-interval", 500*time.Millisecond, "intervalo dos health gates")
	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("argumentos posicionais inesperados: %v", flags.Args())
	}
	if config.HealthInterval <= 0 || config.StopTimeout <= 0 || config.HealthTimeout <= 0 {
		return Config{}, fmt.Errorf("timeouts e intervalos devem ser positivos")
	}
	if operation != OperationPrepare && config.ManifestPath == "" && config.Execute {
		return Config{}, fmt.Errorf("--manifest é obrigatório com --execute")
	}
	return config, nil
}

func printPlan(operation Operation, execute bool, plan []Step) {
	mode := "DRY-RUN"
	if execute {
		mode = "EXECUÇÃO SOLICITADA"
	}
	fmt.Printf("safe-rollout %s [%s]\n", operation, mode)
	for index, step := range plan {
		mutation := ""
		if step.Mutating {
			mutation = " [mutação]"
		}
		fmt.Printf("%d. %s%s — %s\n", index+1, step.Name, mutation, step.Description)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "uso: go run ./ops/safe-rollout <prepare|preflight|apply> [flags]")
	fmt.Fprintln(os.Stderr, "padrão: dry-run. apply real exige --execute e --confirm-token.")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "erro:", err)
	os.Exit(1)
}
