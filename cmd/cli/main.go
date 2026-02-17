package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"localhost-magic/internal/storage"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	storePath := storage.DefaultStorePath()

	// Check for custom store path
	for i, arg := range os.Args {
		if arg == "--config" && i+1 < len(os.Args) {
			storePath = os.Args[i+1]
			// Remove these args
			os.Args = append(os.Args[:i], os.Args[i+2:]...)
			break
		}
	}

	store, err := storage.NewStore(storePath)
	if err != nil {
		log.Fatalf("Failed to open store: %v", err)
	}

	command := os.Args[1]

	switch command {
	case "list", "ls":
		cmdList(store)
	case "rename", "mv":
		if len(os.Args) < 4 {
			fmt.Fprintf(os.Stderr, "Usage: localhost-magic rename <old-name> <new-name>\n")
			os.Exit(1)
		}
		cmdRename(store, os.Args[2], os.Args[3])
	case "keep":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "Usage: localhost-magic keep <name> [true|false]\n")
			os.Exit(1)
		}
		keepVal := true
		if len(os.Args) > 3 {
			keepVal = strings.ToLower(os.Args[3]) == "true" || os.Args[3] == "1"
		}
		cmdKeep(store, os.Args[2], keepVal)
	case "blacklist":
		if len(os.Args) < 4 {
			fmt.Fprintf(os.Stderr, "Usage: localhost-magic blacklist <type> <value>\n")
			fmt.Fprintf(os.Stderr, "  type: pid|path|pattern\n")
			os.Exit(1)
		}
		cmdBlacklist(os.Args[2], os.Args[3])
	case "add":
		if len(os.Args) < 4 {
			fmt.Fprintf(os.Stderr, "Usage: localhost-magic add <name> [host:]<port>\n")
			os.Exit(1)
		}
		target := os.Args[3]
		var targetHost string
		var port int
		if idx := strings.LastIndex(target, ":"); idx != -1 {
			// host:port format
			targetHost = target[:idx]
			port, err = strconv.Atoi(target[idx+1:])
			if err != nil {
				log.Fatalf("Invalid port number in %s", target)
			}
		} else {
			// port only, default to 127.0.0.1
			port, err = strconv.Atoi(target)
			if err != nil {
				log.Fatalf("Invalid port number: %s", target)
			}
		}
		cmdAdd(store, os.Args[2], port, targetHost)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("localhost-magic - Manage local service DNS names")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  localhost-magic list                          List all registered services")
	fmt.Println("  localhost-magic rename <old> <new>            Rename a service")
	fmt.Println("  localhost-magic keep <name> [true|false]      Toggle keep status (default: true)")
	fmt.Println("  localhost-magic blacklist <type> <value>      Add to blacklist")
	fmt.Println("  localhost-magic add <name> [host:]<port>       Add manual service entry")
	fmt.Println("  localhost-magic --config <path>               Use custom config path")
	fmt.Println()
	fmt.Println("Arguments:")
	fmt.Println("  <type> for blacklist: pid, path, or pattern")
	fmt.Println("  <value>: PID number, executable path, or regex pattern")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  localhost-magic list")
	fmt.Println("  localhost-magic rename myapp.localhost api.localhost")
	fmt.Println("  localhost-magic keep myapp.localhost")
	fmt.Println("  localhost-magic keep myapp.localhost false")
	fmt.Println("  localhost-magic blacklist pid 12345")
	fmt.Println("  localhost-magic blacklist path /usr/sbin/cupsd")
	fmt.Println("  localhost-magic blacklist pattern '^localhost-magic'")
	fmt.Println("  localhost-magic add myapp.localhost 3000")
	fmt.Println("  localhost-magic add myapp.localhost 192.168.0.1:3000")
}

func cmdList(store *storage.Store) {
	records := store.List()

	if len(records) == 0 {
		fmt.Println("No services registered.")
		fmt.Println("Start the daemon and run some local HTTP services.")
		return
	}

	fmt.Printf("%-30s %-22s %-8s %-6s %s\n", "NAME", "TARGET", "PID", "KEEP", "COMMAND")
	fmt.Println(strings.Repeat("-", 110))

	for _, r := range records {
		cmd := r.ExePath
		if len(r.Args) > 1 {
			cmd = fmt.Sprintf("%s %s", r.ExePath, strings.Join(r.Args[1:], " "))
		}
		if len(cmd) > 50 {
			cmd = cmd[:47] + "..."
		}

		markers := ""
		if r.UserDefined {
			markers += "*"
		}
		if r.Keep {
			markers += "K"
		}

		keepStr := ""
		if r.Keep {
			keepStr = "YES"
		}

		target := fmt.Sprintf("%s:%d", r.EffectiveTargetHost(), r.Port)
		fmt.Printf("%-30s %-22s %-8d %-6s %s%s\n", r.Name, target, r.PID, keepStr, markers, cmd)
	}

	fmt.Println()
	fmt.Println("* = user-defined name, K = kept, YES = keep enabled")
}

func cmdRename(store *storage.Store, oldName, newName string) {
	// Ensure .localhost suffix
	if !strings.HasSuffix(oldName, ".localhost") {
		oldName = oldName + ".localhost"
	}
	if !strings.HasSuffix(newName, ".localhost") {
		newName = newName + ".localhost"
	}

	// Find the service
	record, ok := store.GetByName(oldName)
	if !ok {
		log.Fatalf("Service not found: %s", oldName)
	}

	// Check if new name is available
	if _, exists := store.GetByName(newName); exists {
		log.Fatalf("Name already in use: %s", newName)
	}

	// Perform rename
	if err := store.UpdateName(record.ID, newName); err != nil {
		log.Fatalf("Failed to rename: %v", err)
	}

	fmt.Printf("Renamed %s -> %s\n", oldName, newName)
	fmt.Println("Note: You may need to restart the daemon for changes to take effect.")
}

func cmdKeep(store *storage.Store, name string, keep bool) {
	// Ensure .localhost suffix
	if !strings.HasSuffix(name, ".localhost") {
		name = name + ".localhost"
	}

	// Find the service
	record, ok := store.GetByName(name)
	if !ok {
		log.Fatalf("Service not found: %s", name)
	}

	// Update keep status
	if err := store.UpdateKeep(record.ID, keep); err != nil {
		log.Fatalf("Failed to update keep status: %v", err)
	}

	status := "enabled"
	if !keep {
		status = "disabled"
	}
	fmt.Printf("Keep %s for %s\n", status, name)
	fmt.Println("Note: You may need to restart the daemon for changes to take effect.")
}

func cmdBlacklist(blacklistType, value string) {
	// Validate type
	validTypes := map[string]bool{"pid": true, "path": true, "pattern": true}
	if !validTypes[blacklistType] {
		log.Fatalf("Invalid blacklist type: %s (must be pid, path, or pattern)", blacklistType)
	}

	// For now, just log the request (daemon needs to implement persistent blacklist)
	fmt.Printf("Blacklist %s: %s\n", blacklistType, value)
	fmt.Println("Note: This will be applied when the daemon restarts.")
	fmt.Println("      To blacklist immediately, use the web dashboard.")
}

func cmdAdd(store *storage.Store, name string, port int, targetHost string) {
	// Ensure .localhost suffix
	if !strings.HasSuffix(name, ".localhost") {
		name = name + ".localhost"
	}

	// Add the manual service
	record, err := store.AddManualService(name, port, targetHost)
	if err != nil {
		log.Fatalf("Failed to add service: %v", err)
	}

	fmt.Printf("Added manual service: %s -> %s:%d\n", record.Name, record.EffectiveTargetHost(), record.Port)
	fmt.Println("Note: This service will be kept even when not running.")
	fmt.Println("      Restart the daemon to activate the proxy.")
}
