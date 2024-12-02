// Copyright 2024 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/syzkaller/pkg/ast"
	"github.com/google/syzkaller/pkg/clangtool"
	"github.com/google/syzkaller/pkg/compiler"
	"github.com/google/syzkaller/pkg/declextract"
	"github.com/google/syzkaller/pkg/mgrconfig"
	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/pkg/subsystem"
	_ "github.com/google/syzkaller/pkg/subsystem/lists"
	"github.com/google/syzkaller/pkg/tool"
	"github.com/google/syzkaller/sys/targets"
)

// The target we currently assume for extracted descriptions.
var target = targets.Get(targets.Linux, targets.AMD64)

func main() {
	var (
		flagConfig       = flag.String("config", "", "manager config file")
		flagBinary       = flag.String("binary", "syz-declextract", "path to syz-declextract binary")
		flagCacheExtract = flag.Bool("cache-extract", false, "use cached extract results if present"+
			" (cached in manager.workdir/declextract.cache)")
	)
	defer tool.Init()()
	cfg, err := mgrconfig.LoadFile(*flagConfig)
	if err != nil {
		tool.Fail(err)
	}
	if err := run(filepath.FromSlash("sys/linux/auto.txt"), &clangtool.Config{
		ToolBin:    *flagBinary,
		KernelSrc:  cfg.KernelSrc,
		KernelObj:  cfg.KernelObj,
		CacheDir:   filepath.Join(cfg.Workdir, "declextract.cache"),
		ReuseCache: *flagCacheExtract,
	}); err != nil {
		tool.Fail(err)
	}
}

func run(autoFile string, cfg *clangtool.Config) error {
	syscallRename, err := buildSyscallRenameMap(cfg.KernelSrc)
	if err != nil {
		return fmt.Errorf("failed to build syscall rename map: %w", err)
	}
	out, err := clangtool.Run(cfg)
	if err != nil {
		return err
	}
	descriptions, interfaces, err := declextract.Run(out, syscallRename)
	if err != nil {
		return err
	}
	if err := osutil.WriteFile(autoFile, descriptions); err != nil {
		return err
	}
	if err := osutil.WriteFile(autoFile+".info", serialize(interfaces)); err != nil {
		return err
	}
	// In order to remove unused bits of the descriptions, we need to write them out first,
	// and then parse all descriptions back b/c auto descriptions use some types defined
	// by manual descriptions (compiler.CollectUnused requires complete descriptions).
	// This also canonicalizes them b/c new lines are added during parsing.
	eh, errors := errorHandler()
	desc := ast.ParseGlob(filepath.Join(filepath.Dir(autoFile), "*.txt"), eh)
	if desc == nil {
		return fmt.Errorf("failed to parse descriptions\n%s", errors.Bytes())
	}
	// Need to clone descriptions b/c CollectUnused changes them slightly during type checking.
	unusedNodes, err := compiler.CollectUnused(desc.Clone(), target, eh)
	if err != nil {
		return fmt.Errorf("failed to typecheck descriptions: %w\n%s", err, errors.Bytes())
	}
	consts := compiler.ExtractConsts(desc.Clone(), target, eh)
	if consts == nil {
		return fmt.Errorf("failed to typecheck descriptions: %w\n%s", err, errors.Bytes())
	}
	finishInterfaces(interfaces, consts, autoFile)
	if err := osutil.WriteFile(autoFile+".info", serialize(interfaces)); err != nil {
		return err
	}
	unused := make(map[string]bool)
	for _, n := range unusedNodes {
		_, typ, name := n.Info()
		unused[typ+name] = true
	}
	desc.Nodes = slices.DeleteFunc(desc.Nodes, func(n ast.Node) bool {
		pos, typ, name := n.Info()
		return pos.File != autoFile || unused[typ+name]
	})
	// We need re-parse them again b/c new lines are fixed up during parsing.
	formatted := ast.Format(ast.Parse(ast.Format(desc), autoFile, nil))
	return osutil.WriteFile(autoFile, formatted)
}

func errorHandler() (func(pos ast.Pos, msg string), *bytes.Buffer) {
	errors := new(bytes.Buffer)
	eh := func(pos ast.Pos, msg string) {
		pos.File = filepath.Base(pos.File)
		fmt.Fprintf(errors, "%v: %v\n", pos, msg)
	}
	return eh, errors
}

func serialize(interfaces []*declextract.Interface) []byte {
	w := new(bytes.Buffer)
	for _, iface := range interfaces {
		fmt.Fprintf(w, "%v\t%v\tfunc:%v\taccess:%v\tmanual_desc:%v\tauto_desc:%v",
			iface.Type, iface.Name, iface.Func, iface.Access,
			iface.ManualDescriptions, iface.AutoDescriptions)
		for _, file := range iface.Files {
			fmt.Fprintf(w, "\tfile:%v", file)
		}
		for _, subsys := range iface.Subsystems {
			fmt.Fprintf(w, "\tsubsystem:%v", subsys)
		}
		fmt.Fprintf(w, "\n")
	}
	return w.Bytes()
}

func finishInterfaces(interfaces []*declextract.Interface, consts map[string]*compiler.ConstInfo, autoFile string) {
	manual := make(map[string]bool)
	for file, desc := range consts {
		for _, c := range desc.Consts {
			if file != autoFile {
				manual[c.Name] = true
			}
		}
	}
	extractor := subsystem.MakeExtractor(subsystem.GetList(target.OS))
	for _, iface := range interfaces {
		iface.ManualDescriptions = manual[iface.IdentifyingConst]
		var crashes []*subsystem.Crash
		for _, file := range iface.Files {
			crashes = append(crashes, &subsystem.Crash{GuiltyPath: file})
		}
		for _, s := range extractor.Extract(crashes) {
			iface.Subsystems = append(iface.Subsystems, s.Name)
		}
		slices.Sort(iface.Subsystems)
	}
}

func buildSyscallRenameMap(sourceDir string) (map[string][]string, error) {
	// Some syscalls have different names and entry points and thus need to be renamed.
	// e.g. SYSCALL_DEFINE1(setuid16, old_uid_t, uid) is referred to in the .tbl file with setuid.
	// Parse *.tbl files that map functions defined with SYSCALL_DEFINE macros to actual syscall names.
	// Lines in the files look as follows:
	//	288      common  accept4                 sys_accept4
	// Total mapping is many-to-many, so we give preference to x86 arch, then to 64-bit syscalls,
	// and then just order arches by name to have deterministic result.
	// Note: some syscalls may have no record in the tables for the architectures we support.
	syscalls := make(map[string][]tblSyscall)
	tblFiles, err := findTblFiles(sourceDir)
	if err != nil {
		return nil, err
	}
	if len(tblFiles) == 0 {
		return nil, fmt.Errorf("found no *.tbl files in the kernel dir %v", sourceDir)
	}
	for file, arches := range tblFiles {
		for _, arch := range arches {
			data, err := os.ReadFile(file)
			if err != nil {
				return nil, err
			}
			parseTblFile(data, arch, syscalls)
		}
	}
	rename := make(map[string][]string)
	for syscall, descs := range syscalls {
		slices.SortFunc(descs, func(a, b tblSyscall) int {
			if (a.arch == target.Arch) != (b.arch == target.Arch) {
				if a.arch == target.Arch {
					return -1
				}
				return 1
			}
			if a.is64bit != b.is64bit {
				if a.is64bit {
					return -1
				}
				return 1
			}
			return strings.Compare(a.arch, b.arch)
		})
		fn := descs[0].fn
		rename[fn] = append(rename[fn], syscall)
	}
	return rename, nil
}

type tblSyscall struct {
	fn      string
	arch    string
	is64bit bool
}

func parseTblFile(data []byte, arch string, syscalls map[string][]tblSyscall) {
	for s := bufio.NewScanner(bytes.NewReader(data)); s.Scan(); {
		fields := strings.Fields(s.Text())
		if len(fields) < 4 || fields[0] == "#" {
			continue
		}
		group := fields[1]
		syscall := fields[2]
		fn := strings.TrimPrefix(fields[3], "sys_")
		if strings.HasPrefix(syscall, "unused") || fn == "-" ||
			// Powerpc spu group defines some syscalls (utimesat)
			// that are not present on any of our arches.
			group == "spu" ||
			// llseek does not exist, it comes from:
			//	arch/arm64/tools/syscall_64.tbl -> scripts/syscall.tbl
			//	62  32      llseek                          sys_llseek
			// So scripts/syscall.tbl is pulled for 64-bit arch, but the syscall
			// is defined only for 32-bit arch in that file.
			syscall == "llseek" ||
			// Don't want to test it (but see issue 5308).
			syscall == "reboot" {
			continue
		}
		syscalls[syscall] = append(syscalls[syscall], tblSyscall{
			fn:      fn,
			arch:    arch,
			is64bit: group == "common" || strings.Contains(group, "64"),
		})
	}
}

func findTblFiles(sourceDir string) (map[string][]string, error) {
	files := make(map[string][]string)
	for _, arch := range targets.List[target.OS] {
		err := filepath.Walk(filepath.Join(sourceDir, "arch", arch.KernelHeaderArch),
			func(file string, info fs.FileInfo, err error) error {
				if err == nil && strings.HasSuffix(file, ".tbl") {
					files[file] = append(files[file], arch.VMArch)
				}
				return err
			})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}