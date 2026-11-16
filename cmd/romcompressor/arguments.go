package main

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	romcompressor "github.com/olsonb97/romcompressor.go"
)

type arguments struct {
	command   string
	paths     []string
	format    string
	output    string
	entry     string
	media     string
	keys      string
	tempDir   string
	maxOutput int64
	tools     map[string]string
	json      bool
	help      bool
	seen      map[string]bool
}

func parse(argv []string) (arguments, error) {
	if len(argv) > 0 && argv[0] == "--version" {
		argv = append([]string{"version"}, argv[1:]...)
	}
	a := arguments{tools: make(map[string]string), seen: make(map[string]bool), json: requestsJSON(argv)}
	if len(argv) == 0 {
		a.help = true
		return a, nil
	}
	if argv[0] == "-h" || argv[0] == "--help" || argv[0] == "help" {
		a.help = true
		if len(argv) == 2 {
			a.command = argv[1]
		} else if len(argv) > 2 {
			return a, errors.New("help accepts one command")
		}
		if a.command != "" && !knownCommand(a.command) {
			return a, fmt.Errorf("unknown command %q", a.command)
		}
		return a, nil
	}
	a.command = argv[0]
	if !knownCommand(a.command) {
		return a, fmt.Errorf("unknown command %q; run romcompressor --help", a.command)
	}
	positionalOnly := false
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		if positionalOnly || !strings.HasPrefix(arg, "-") || arg == "-" {
			a.paths = append(a.paths, arg)
			continue
		}
		if arg == "--" {
			positionalOnly = true
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		name = canonicalFlag(name)
		if name == "" {
			return a, fmt.Errorf("unknown option %q", arg)
		}
		if a.seen[name] && name != "tool" {
			return a, fmt.Errorf("option --%s was specified twice", name)
		}
		a.seen[name] = true
		if switchFlag(name) {
			if hasValue {
				return a, fmt.Errorf("--%s does not take a value", name)
			}
			switch name {
			case "help":
				a.help = true
			case "json":
				a.json = true
			}
			continue
		}
		if !hasValue {
			i++
			if i == len(argv) || optionToken(argv[i]) {
				return a, fmt.Errorf("--%s requires a value", name)
			}
			value = argv[i]
		}
		if strings.TrimSpace(value) == "" {
			return a, fmt.Errorf("--%s requires a nonempty value", name)
		}
		switch name {
		case "format":
			a.format = strings.ToLower(value)
		case "output":
			a.output = value
		case "entry":
			a.entry = value
		case "media":
			a.media = strings.ToLower(value)
		case "keys":
			a.keys = value
		case "temp-dir":
			a.tempDir = value
		case "max-output":
			limit, err := parseByteLimit(value)
			if err != nil {
				return a, err
			}
			a.maxOutput = limit
		case "tool":
			engine, path, ok := strings.Cut(value, "=")
			if !ok || path == "" || !knownEngine(engine) {
				return a, errors.New("--tool expects ENGINE=PATH; engines are chdman, dolphin-tool, nsz, 7z, jwudtool, java, azahar, psxpackager, and zarchive")
			}
			if _, exists := a.tools[engine]; exists {
				return a, fmt.Errorf("tool %q was specified twice", engine)
			}
			a.tools[engine] = path
		}
	}
	if a.help {
		return a, nil
	}
	return a, validateArguments(a)
}

// Select the error format even when an earlier argument fails validation. Flag
// values and filenames following -- must not be mistaken for output options.
func requestsJSON(argv []string) bool {
	for i := 1; i < len(argv); i++ {
		if argv[i] == "--" {
			break
		}
		if argv[i] == "--json" || argv[i] == "-j" {
			return true
		}
		flag, _, hasValue := strings.Cut(argv[i], "=")
		name := canonicalFlag(flag)
		if name != "" && !switchFlag(name) && !hasValue && i+1 < len(argv) && !optionToken(argv[i+1]) {
			i++
		}
	}
	return false
}

// Recognized options cannot accidentally become values for preceding options.
// Literal flag-like paths can still be passed with --output=-j, for example.
func optionToken(value string) bool {
	flag, _, _ := strings.Cut(value, "=")
	return strings.HasPrefix(flag, "--") || canonicalFlag(flag) != ""
}

func parseByteLimit(value string) (int64, error) {
	number, multiplier := value, int64(1)
	for _, unit := range []struct {
		suffix string
		bytes  int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}} {
		if strings.HasSuffix(strings.ToLower(value), strings.ToLower(unit.suffix)) {
			number = strings.TrimSpace(value[:len(value)-len(unit.suffix)])
			multiplier = unit.bytes
			break
		}
	}
	bytes, err := strconv.ParseInt(number, 10, 64)
	if err != nil || bytes <= 0 || bytes > math.MaxInt64/multiplier {
		return 0, errors.New("--max-output requires a positive whole byte count or a size such as 4GiB (KiB, MiB, GiB, TiB)")
	}
	return bytes * multiplier, nil
}

func canonicalFlag(flag string) string {
	switch flag {
	case "-h", "--help":
		return "help"
	case "-j", "--json":
		return "json"
	case "-n", "--dry-run":
		return "dry-run"
	case "--all":
		return "all"
	case "-f", "--format":
		return "format"
	case "-o", "--output":
		return "output"
	case "-s", "--entry":
		return "entry"
	case "--media":
		return "media"
	case "--keys":
		return "keys"
	case "--temp-dir":
		return "temp-dir"
	case "--max-output":
		return "max-output"
	case "--tool":
		return "tool"
	}
	return ""
}

func switchFlag(name string) bool {
	switch name {
	case "help", "json", "dry-run", "all":
		return true
	}
	return false
}

func knownCommand(command string) bool {
	switch command {
	case "compress", "decompress", "trim", "inspect", "verify", "formats", "plan", "batch", "version":
		return true
	}
	return false
}

func knownEngine(engine string) bool {
	switch engine {
	case "chdman", "dolphin-tool", "nsz", "7z", "jwudtool", "java", "azahar", "psxpackager", "zarchive":
		return true
	}
	return false
}

func validateArguments(a arguments) error {
	allowed := map[string]string{
		"compress":   " format output media keys max-output tool json ",
		"decompress": " output entry all media keys max-output tool json ",
		"trim":       " output dry-run json ",
		"inspect":    " tool json ",
		"verify":     " entry media keys max-output tool temp-dir json ",
		"formats":    " tool json ",
		"plan":       " max-output tool json ",
		"batch":      " tool keys json ",
		"version":    " json ",
	}
	for flag := range a.seen {
		if !strings.Contains(allowed[a.command], " "+flag+" ") {
			return fmt.Errorf("--%s is not valid for %s", flag, a.command)
		}
	}
	wanted := 1
	if a.command == "formats" || a.command == "version" {
		wanted = 0
	}
	if len(a.paths) != wanted {
		return fmt.Errorf("%s requires %d file argument(s); run romcompressor %s --help", a.command, wanted, a.command)
	}
	if wanted != 0 && (a.paths[0] == "-" || a.paths[0] == "") {
		return errors.New("provide a file path; this command requires seekable files")
	}
	if a.output == "-" {
		return errors.New("provide an output path so the result can be verified before publication")
	}
	if a.seen["dry-run"] && a.output != "" {
		return errors.New("--dry-run does not take an output path")
	}
	if a.seen["all"] && (a.output == "" || a.entry != "" || a.media != "" || a.keys != "") {
		return errors.New("--all requires an output directory and excludes entry, media, and keys options")
	}
	if a.command == "compress" {
		if a.format == "" {
			return errors.New("choose an output format with -f; run romcompressor formats")
		}
		if a.format == "raw" {
			return errors.New("use decompress to produce an uncompressed file")
		}
		valid := false
		for _, format := range romcompressor.Formats() {
			if string(format.Format) == a.format {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("unknown format %q; run romcompressor formats", a.format)
		}
	}
	if a.media != "" && a.media != "cd" && a.media != "dvd" && a.media != "hdd" {
		return errors.New("--media must be cd, dvd, or hdd")
	}
	return nil
}
