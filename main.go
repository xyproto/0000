package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/xyproto/distrodetector"
)

const version = "2.0.7"

type Options struct {
	CXX               string
	Std               string
	Win64Docker       bool
	Debug             bool
	Strict            bool
	Sloppy            bool
	Opt               bool
	Clang             bool
	Run               bool
	Test              bool
	Clean             bool
	Pro               bool
	Version           bool
	MainSource        string
	OutputName        string
	DetectedDistro    string
	Sources           []string
	TestSources       []string
	IncludeDirs       []string
	SystemIncludeDirs []string
	ExtraCFlags       []string
	ExtraLDFlags      []string
}

type CompileCache struct {
	Timestamps map[string]int64 `json:"timestamps"`
}

var stdIncludesSkipList = []string{
	"algorithm", "array", "barrier", "bit", "bitset", "cassert", "ccomplex",
	"cctype", "cerrno", "cfenv", "cfloat", "chrono", "cinttypes", "ciso646",
	"climits", "clocale", "cmath", "codecvt", "complex", "condition_variable",
	"coroutine", "cstdio", "cstdlib", "cstring", "ctime", "cwchar", "cwctype",
	"deque", "exception", "execution", "filesystem", "format", "forward_list",
	"fstream", "functional", "future", "initializer_list", "iomanip", "ios",
	"iosfwd", "iostream", "istream", "iterator", "latch", "limits", "list",
	"locale", "map", "memory", "mutex", "new", "numbers", "numeric", "optional",
	"ostream", "queue", "random", "ranges", "ratio", "regex", "scoped_allocator",
	"semaphore", "set", "shared_mutex", "source_location", "span", "sstream",
	"stack", "stdexcept", "steambuf", "stop_token", "streambuf", "string",
	"string_view", "strstream", "syncstream", "system_error", "tgmath",
	"thread", "tuple", "type_traits", "typeindex", "typeinfo", "unordered_map",
	"unordered_set", "utility", "valarray", "variant", "vector", "version",
	"atomic",
}

func main() {
	opts := parseArgs()
	if opts.Version {
		fmt.Printf("cxx2 version %s\n", version)
		return
	}
	distro := distrodetector.New()
	opts.DetectedDistro = distro.String()
	adjustCompiler(opts)

	srcs, err := discoverSources()
	if err != nil {
		log.Fatal(err)
	}
	if len(srcs) == 0 && !opts.Clean {
		fmt.Println("No sources found.")
		return
	}

	var normalSources, testSources []string
	for _, s := range srcs {
		if isTestSource(s) {
			testSources = append(testSources, s)
		} else {
			normalSources = append(normalSources, s)
		}
	}
	opts.Sources = srcs
	opts.TestSources = testSources
	opts.MainSource = findMainSource(srcs)

	if opts.MainSource != "" {
		opts.OutputName = guessOutputNameFromMain(opts.MainSource, opts.Win64Docker)
	} else if len(normalSources) > 0 {
		out := "main"
		if opts.Win64Docker {
			out += ".exe"
		} else if runtime.GOOS == "windows" {
			out += ".exe"
		}
		opts.OutputName = out
	}

	if opts.Clean {
		removeArtifacts(opts)
		return
	}

	opts.SystemIncludeDirs = discoverSystemIncludeDirs()
	opts.IncludeDirs = discoverLocalIncludeDirs()

	incls := gatherAllIncludes(opts.Sources)
	missing := checkMissingHeaders(incls, opts)
	if len(missing) > 0 {
		pkgDiscovery(opts, missing)
	}

	if opts.Pro {
		if err := generateProFile(opts, normalSources); err != nil {
			fmt.Println("Could not generate .pro:", err)
		}
		return
	}

	cc, _ := loadCache()

	// If there's exactly 1 normal source, no test sources, do single-step build (no partial detection).
	if len(normalSources) == 1 && len(testSources) == 0 && !opts.Test {
		if err := singleStepBuild(opts, normalSources[0]); err != nil {
			log.Fatal("Build error:", err)
		}
	} else {
		if err := compileAndLink(opts, cc); err != nil {
			log.Fatal("Build error:", err)
		}
		saveCache(cc)
	}

	if opts.Test && len(testSources) > 0 {
		if err := buildAndRunTests(opts, cc); err != nil {
			log.Fatal("Test error:", err)
		}
	}

	if opts.Run && opts.OutputName != "" {
		fmt.Println("Running:", opts.OutputName)
		if opts.Win64Docker {
			fmt.Println("Cross-compiled .exe can't be run automatically under Docker.")
		} else {
			cmd := exec.Command("./" + opts.OutputName)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				log.Fatal(err)
			}
		}
	}
	fmt.Printf("Build complete on %s\n", opts.DetectedDistro)
}

func parseArgs() *Options {
	o := &Options{CXX: "g++", Std: "c++20"}
	for _, arg := range os.Args[1:] {
		switch arg {
		case "run":
			o.Run = true
		case "test":
			o.Test = true
		case "clean":
			o.Clean = true
		case "pro":
			o.Pro = true
		case "--version", "version":
			o.Version = true
		case "debug":
			o.Debug = true
		case "strict":
			o.Strict = true
		case "sloppy":
			o.Sloppy = true
		case "opt":
			o.Opt = true
		case "clang":
			o.Clang = true
		case "--win64-docker":
			o.Win64Docker = true
			o.CXX = "x86_64-w64-mingw32-g++"
		default:
			if strings.HasPrefix(arg, "--cxx=") {
				o.CXX = strings.TrimPrefix(arg, "--cxx=")
			} else if strings.HasPrefix(arg, "cxx=") {
				o.CXX = strings.TrimPrefix(arg, "cxx=")
			}
		}
	}
	return o
}

func adjustCompiler(o *Options) {
	if o.Clang && !o.Win64Docker {
		o.CXX = "clang++"
	}
}

func discoverSources() ([]string, error) {
	var out []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, e error) error {
		if e != nil || d.IsDir() {
			if d != nil && d.IsDir() && strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
				return filepath.SkipDir
			}
			return nil
		}
		l := strings.ToLower(path)
		if strings.HasSuffix(l, ".c") || strings.HasSuffix(l, ".cc") ||
			strings.HasSuffix(l, ".cpp") || strings.HasSuffix(l, ".cxx") {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

func isTestSource(s string) bool {
	l := strings.ToLower(filepath.Base(s))
	if strings.HasSuffix(l, "_test.cpp") || strings.HasSuffix(l, "_test.cc") ||
		strings.HasSuffix(l, "_test.cxx") || strings.HasSuffix(l, "_test.c") {
		return true
	}
	switch l {
	case "test.cpp", "test.cc", "test.cxx", "test.c":
		return true
	}
	return false
}

func findMainSource(srcs []string) string {
	for _, s := range srcs {
		l := strings.ToLower(filepath.Base(s))
		if l == "main.cpp" || l == "main.cc" || l == "main.cxx" || l == "main.c" {
			return s
		}
	}
	var nt []string
	for _, s := range srcs {
		if !isTestSource(s) {
			nt = append(nt, s)
		}
	}
	if len(nt) == 1 {
		if b, e := os.ReadFile(nt[0]); e == nil && strings.Contains(string(b), " main(") {
			return nt[0]
		}
		return nt[0]
	}
	for _, s := range nt {
		if b, e := os.ReadFile(s); e == nil && strings.Contains(string(b), " main(") {
			return s
		}
	}
	return ""
}

func guessOutputNameFromMain(mainSrc string, docker bool) string {
	dir, _ := os.Getwd()
	b := filepath.Base(dir)
	if b == "src" {
		b = strings.TrimSuffix(filepath.Base(mainSrc), filepath.Ext(mainSrc))
		if b == "" {
			b = "main"
		}
	}
	if docker && !strings.HasSuffix(b, ".exe") {
		b += ".exe"
	} else if runtime.GOOS == "windows" && !strings.HasSuffix(b, ".exe") {
		b += ".exe"
	}
	return b
}

func removeArtifacts(o *Options) {
	filepath.WalkDir(".", func(p string, d fs.DirEntry, e error) error {
		if e != nil || d.IsDir() {
			return nil
		}
		l := strings.ToLower(d.Name())
		if strings.HasSuffix(l, ".o") || strings.HasSuffix(l, ".obj") {
			fmt.Printf("Removing %s\n", p)
			os.Remove(p)
		}
		return nil
	})
	if o.OutputName != "" && fileExists(o.OutputName) {
		fmt.Printf("Removing %s\n", o.OutputName)
		os.Remove(o.OutputName)
	}
	if fileExists(".cxxcache") {
		fmt.Println("Removing .cxxcache")
		os.Remove(".cxxcache")
	}
}

func gatherAllIncludes(files []string) []string {
	s := map[string]bool{}
	for _, f := range files {
		for _, inc := range discoverIncludes(f) {
			s[inc] = true
		}
	}
	var out []string
	for inc := range s {
		out = append(out, inc)
	}
	return out
}

func discoverIncludes(file string) []string {
	b, e := os.ReadFile(file)
	if e != nil {
		return nil
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	rx := regexp.MustCompile(`^\s*#\s*include\s*["<]([^">]+)[">]`)
	for sc.Scan() {
		line := sc.Text()
		if m := rx.FindStringSubmatch(line); len(m) == 2 {
			out = append(out, m[1])
		}
	}
	return out
}

func fileExists(p string) bool {
	i, e := os.Stat(p)
	return e == nil && i.Mode().IsRegular()
}

func isStdInclude(header string) bool {
	h := strings.ToLower(strings.TrimSuffix(header, filepath.Ext(header)))
	h = strings.TrimPrefix(h, "c")
	for _, s := range stdIncludesSkipList {
		if h == s {
			return true
		}
	}
	return false
}

func checkMissingHeaders(includes []string, o *Options) []string {
	var out []string
LOOP:
	for _, inc := range includes {
		if isStdInclude(inc) {
			continue
		}
		for _, d := range o.IncludeDirs {
			if fileExists(filepath.Join(d, inc)) {
				continue LOOP
			}
		}
		for _, d := range o.SystemIncludeDirs {
			if fileExists(filepath.Join(d, inc)) {
				continue LOOP
			}
		}
		out = append(out, inc)
	}
	return out
}

func pkgDiscovery(o *Options, missing []string) {
	fmt.Println("Missing headers:")
	for _, h := range missing {
		fmt.Println("  ", h)
		pkg, cmd := mapHeaderToPkg(h, o.DetectedDistro)
		if pkg != "" && cmd != "" {
			fmt.Printf("    Possibly install with: %s\n", cmd)
			if flags, err := gatherPkgConfigFlags(pkg, o.DetectedDistro); err == nil && flags != "" {
				mergePkgConfigFlags(flags, o)
			}
		}
	}
	if !o.Sloppy {
		fmt.Println("\nCannot proceed unless sloppy mode is used or you fix missing headers.")
		os.Exit(1)
	} else {
		fmt.Println("Continuing in sloppy mode, ignoring missing headers.")
	}
}

func discoverSystemIncludeDirs() []string {
	d := []string{"/usr/include", "/usr/local/include"}
	if fileExists("/usr/include/x86_64-linux-gnu") {
		d = append(d, "/usr/include/x86_64-linux-gnu")
	}
	return d
}

func discoverLocalIncludeDirs() []string {
	d := []string{"include", ".", "common"}
	if fileExists("../include") {
		d = append(d, "../include")
	}
	if fileExists("../common") {
		d = append(d, "../common")
	}
	return d
}

func loadCache() (*CompileCache, error) {
	cc := &CompileCache{Timestamps: map[string]int64{}}
	b, e := os.ReadFile(".cxxcache")
	if e == nil {
		_ = json.Unmarshal(b, cc)
	}
	return cc, nil
}

func saveCache(cc *CompileCache) {
	b, _ := json.MarshalIndent(cc, "", "  ")
	_ = os.WriteFile(".cxxcache", b, 0o644)
}

// singleStepBuild: just one normal source, no tests -> compile and link in one g++ step
func singleStepBuild(o *Options, source string) error {
	on := ensureExeSuffix(o.OutputName, o.Win64Docker)
	flags := compileFlags(o)
	sf := ""
	if o.Std != "" {
		sf = "-std=" + o.Std
	}
	cf := joinExtraCFlags(o.ExtraCFlags)
	linkFlags := joinExtraLDFlags(o.ExtraLDFlags)
	line := fmt.Sprintf(`%s %s %s %s %s -o %s`,
		o.CXX, sf, flags, cf, source, on)
	if linkFlags != "" {
		line += " " + linkFlags
	}
	fmt.Println(line)
	if e := runCommand(line, o); e != nil {
		return e
	}
	o.OutputName = on
	return nil
}

func compileAndLink(o *Options, cc *CompileCache) error {
	var objs []string
	for _, s := range o.Sources {
		if !o.Test && isTestSource(s) {
			continue
		}
		obj, e := compileOne(o, cc, s)
		if e != nil {
			return e
		}
		objs = append(objs, obj)
	}
	on := ensureExeSuffix(o.OutputName, o.Win64Docker)
	if e := linkObjects(o, objs, on); e != nil {
		return e
	}
	o.OutputName = on
	return nil
}

func ensureExeSuffix(base string, docker bool) string {
	if docker && !strings.HasSuffix(base, ".exe") {
		return base + ".exe"
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(base, ".exe") {
		return base + ".exe"
	}
	return base
}

func compileOne(o *Options, cc *CompileCache, src string) (string, error) {
	ext := filepath.Ext(src)
	obj := strings.TrimSuffix(filepath.Base(src), ext) + ".o"
	if needsRebuild(src, obj, cc) {
		line := buildCompileCmd(o, src, obj)
		if err := runCommand(line, o); err != nil {
			return obj, err
		}
		updateTimestamp(src, cc)
	}
	return obj, nil
}

func buildCompileCmd(o *Options, src, obj string) string {
	flags := compileFlags(o)
	sf := ""
	if o.Std != "" {
		sf = "-std=" + o.Std
	}
	cf := joinExtraCFlags(o.ExtraCFlags)
	return fmt.Sprintf(`%s %s %s %s -c %s -o %s`,
		o.CXX, sf, flags, cf, src, obj)
}

func compileFlags(o *Options) string {
	baseFlags := []string{
		"-pipe",
		"-fPIC",
		"-fno-plt",
		"-fstack-protector-strong",
		"-Wall",
		"-Wshadow",
		"-Wpedantic",
		"-Wno-parentheses",
		"-Wfatal-errors",
		"-Wvla",
		"-Wignored-qualifiers",
	}
	if o.Debug {
		baseFlags = removeFromSlice(baseFlags, "-O2")
		baseFlags = append(baseFlags, "-O0", "-g")
	} else if o.Opt {
		baseFlags = append(baseFlags, "-O2")
	}
	if o.Strict {
		baseFlags = append(baseFlags, "-Wextra", "-Wconversion")
	}
	if o.Sloppy {
		baseFlags = append(baseFlags, "-w", "-fpermissive")
	}
	return strings.Join(baseFlags, " ")
}

func removeFromSlice(sl []string, val string) []string {
	var out []string
	for _, s := range sl {
		if s != val {
			out = append(out, s)
		}
	}
	return out
}

func joinExtraCFlags(flags []string) string {
	if len(flags) == 0 {
		return ""
	}
	return strings.Join(flags, " ")
}

func linkObjects(o *Options, objs []string, out string) error {
	flags := compileFlags(o)
	linkFlags := joinExtraLDFlags(o.ExtraLDFlags)
	line := fmt.Sprintf(`%s %s %s -o %s`,
		o.CXX, flags, strings.Join(objs, " "), out)
	if linkFlags != "" {
		line += " " + linkFlags
	}
	return runCommand(line, o)
}

func joinExtraLDFlags(ldflags []string) string {
	if len(ldflags) == 0 {
		return ""
	}
	return strings.Join(ldflags, " ")
}

func needsRebuild(src, obj string, cc *CompileCache) bool {
	if !fileExists(obj) {
		return true
	}
	si, e := os.Stat(src)
	if e != nil {
		return true
	}
	oi, e := os.Stat(obj)
	if e != nil || oi.ModTime().Before(si.ModTime()) {
		return true
	}
	old := cc.Timestamps[src]
	newt := si.ModTime().Unix()
	return old != newt
}

func updateTimestamp(src string, cc *CompileCache) {
	if i, e := os.Stat(src); e == nil {
		cc.Timestamps[src] = i.ModTime().Unix()
	}
}

func buildAndRunTests(o *Options, cc *CompileCache) error {
	var normalObjs []string
	for _, s := range o.Sources {
		if !isTestSource(s) {
			obj, e := compileOne(o, cc, s)
			if e != nil {
				return e
			}
			normalObjs = append(normalObjs, obj)
		}
	}
	for _, s := range o.TestSources {
		obj, e := compileOne(o, cc, s)
		if e != nil {
			return e
		}
		exe := ensureExeSuffix(strings.TrimSuffix(obj, ".o"), o.Win64Docker)
		if err := linkObjects(o, append([]string{obj}, normalObjs...), exe); err != nil {
			return err
		}
		fmt.Println("Running test:", exe)
		if o.Win64Docker {
			fmt.Println("Cannot run Windows .exe test under Docker cross-compile.")
			continue
		}
		cmd := exec.Command("./" + exe)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
	}
	return nil
}

func generateProFile(o *Options, normalSrc []string) error {
	n := strings.TrimSuffix(o.OutputName, ".exe")
	pf := n + ".pro"
	f, e := os.Create(pf)
	if e != nil {
		return e
	}
	defer f.Close()

	var all []string
	all = append(all, normalSrc...)
	if o.MainSource != "" && !contains(all, o.MainSource) {
		all = append(all, o.MainSource)
	}
	fmt.Fprintf(f, "TEMPLATE = app\nCONFIG += c++20\nCONFIG -= console\nCONFIG -= app_bundle\nCONFIG -= qt\n\n")
	fmt.Fprintf(f, "SOURCES += \\\n")
	for i, s := range all {
		if i < len(all)-1 {
			fmt.Fprintf(f, "  %s \\\n", s)
		} else {
			fmt.Fprintf(f, "  %s\n\n", s)
		}
	}
	fmt.Fprintf(f, "INCLUDEPATH += . include ../include ../common\n\n")
	if o.CXX != "" {
		fmt.Fprintf(f, "QMAKE_CXX = %s\n", o.CXX)
	}
	cf := strings.Fields(compileFlags(o))
	extraC := joinExtraCFlags(o.ExtraCFlags)
	if extraC != "" {
		cf = append(cf, extraC)
	}
	if len(cf) > 0 {
		fmt.Fprintf(f, "QMAKE_CXXFLAGS += %s\n", strings.Join(cf, " "))
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func runCommand(line string, o *Options) error {
	fmt.Println(line)
	if o.Win64Docker {
		p := strings.Fields(line)
		if len(p) == 0 {
			return nil
		}
		img := "jhasse/mingw:latest"
		a := []string{"run", "-v", fmt.Sprintf("%s:/home", mustPwd()), "-w", "/home", "--rm", img}
		a = append(a, p...)
		fmt.Printf("docker %v\n", strings.Join(a, " "))
		c := exec.Command("docker", a...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}
	p := strings.Fields(line)
	if len(p) == 0 {
		return nil
	}
	c := exec.Command(p[0], p[1:]...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func mustPwd() string {
	w, e := os.Getwd()
	if e != nil {
		panic(e)
	}
	return w
}

func gatherPkgConfigFlags(pkg, distro string) (string, error) {
	if !haveCmd("pkg-config") {
		return "", fmt.Errorf("pkg-config not found")
	}
	cmdStr := "pkg-config --cflags --libs " + strings.ToLower(pkg)
	out, err := runShellCommand(cmdStr)
	if err != nil || out == "" {
		return "", fmt.Errorf("no pkg-config info for %s", pkg)
	}
	return strings.TrimSpace(out), nil
}

func runShellCommand(cmd string) (string, error) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return "", fmt.Errorf("empty command")
	}
	c := exec.Command(parts[0], parts[1:]...)
	b, err := c.CombinedOutput()
	return string(b), err
}

func haveCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func mergePkgConfigFlags(flags string, o *Options) {
	fs := strings.Fields(flags)
	for _, f := range fs {
		if strings.HasPrefix(f, "-I") || strings.HasPrefix(f, "-D") || strings.HasPrefix(f, "-F") ||
			strings.HasPrefix(f, "-framework") || (strings.HasPrefix(f, "-W") && !strings.HasPrefix(f, "-Wl,")) {
			o.ExtraCFlags = append(o.ExtraCFlags, f)
		} else if strings.HasPrefix(f, "-l") || strings.HasPrefix(f, "-L") ||
			strings.HasPrefix(f, "-Wl,") || strings.HasPrefix(f, "-framework") {
			o.ExtraLDFlags = append(o.ExtraLDFlags, f)
		}
	}
}

func mapHeaderToPkg(h, distro string) (string, string) {
	l := strings.ToLower(h)
	ar := strings.Contains(strings.ToLower(distro), "arch")
	switch {
	case strings.Contains(l, "boost/"):
		if ar {
			return "boost", "pacman -S boost"
		}
		return "libboost-all-dev", "apt install libboost-all-dev"
	case strings.Contains(l, "sdl2/"):
		if ar {
			return "sdl2", "pacman -S sdl2 sdl2_mixer"
		}
		return "libsdl2-dev libsdl2-mixer-dev", "apt install libsdl2-dev libsdl2-mixer-dev"
	case strings.Contains(l, "glm/"):
		if ar {
			return "glm", "pacman -S glm"
		}
		return "libglm-dev", "apt install libglm-dev"
	case strings.Contains(l, "gl.h"), strings.Contains(l, "glu.h"):
		if ar {
			return "mesa", "pacman -S mesa"
		}
		return "mesa-common-dev", "apt install mesa-common-dev"
	case strings.Contains(l, "gtk/gtk.h"):
		if ar {
			return "gtk3", "pacman -S gtk3"
		}
		return "libgtk-3-dev", "apt install libgtk-3-dev"
	case strings.Contains(l, "vulkan/"):
		if ar {
			return "vulkan-devel", "pacman -S vulkan-devel"
		}
		return "libvulkan-dev", "apt install libvulkan-dev"
	}
	return "", ""
}
