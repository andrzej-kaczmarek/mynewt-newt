/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package toolchain

import (
	"bytes"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mynewt.apache.org/newt/util"
)

type DepTracker struct {
	// Most recent .o modification time.
	MostRecent time.Time

	compiler *Compiler
}

func NewDepTracker(c *Compiler) DepTracker {
	tracker := DepTracker{
		MostRecent: time.Unix(0, 0),
		compiler:   c,
	}

	return tracker
}

// @return string               The name of the dependent file (i.e., the first
//                                  .o file encountered).
// @return []string             Populated with the dependencies' filenames.
func parseDepsLine(line string) (string, []string, error) {
	tokens := strings.Fields(line)
	if len(tokens) == 0 {
		return "", nil, nil
	}

	dFileTok := tokens[0]
	if dFileTok[len(dFileTok)-1:] != ":" {
		return "", nil, util.NewNewtError("Invalid Makefile dependency file; " +
			"line missing ':'")
	}

	dFileName := dFileTok[:len(dFileTok)-1]
	return dFileName, tokens[1:], nil

}

// Parses a dependency (.d) file generated by gcc.  On success, the returned
// string array is populated with the dependency filenames.  This function
// expects each line of a dependency file to have the following format:
//
// <file>.o: <file>.c a.h b.h c.h \
//  d.h e.h f.h
//
// Only the first dependent object(<file>.o) is considered.
//
// @return []string             Populated with the dependencies' filenames.
func ParseDepsFile(filename string) ([]string, error) {
	lines, err := util.ReadLines(filename)
	if err != nil {
		return nil, err
	}

	if len(lines) == 0 {
		return []string{}, nil
	}

	var dFile string
	allDeps := []string{}
	for _, line := range lines {
		src, deps, err := parseDepsLine(line)
		if err != nil {
			return nil, err
		}

		if dFile == "" {
			dFile = src
		}

		if src == dFile {
			allDeps = append(allDeps, deps...)
		}
	}

	return allDeps, nil
}

// Updates the dependency tracker's most recent timestamp according to the
// modification time of the specified file.  If the specified file is older
// than the tracker's currently most-recent time, this function has no effect.
func (tracker *DepTracker) ProcessFileTime(file string) error {
	modTime, err := util.FileModificationTime(file)
	if err != nil {
		return err
	}

	if modTime.After(tracker.MostRecent) {
		tracker.MostRecent = modTime
	}

	return nil
}

// Determines if a file was previously built with a command line invocation
// different from the one specified.
//
// @param dstFile               The output file whose build invocation is being
//                                  tested.
// @param cmd                   The command that would be used to generate the
//                                  specified destination file.
//
// @return                      true if the command has changed or if the
//                                  destination file was never built;
//                              false otherwise.
func commandHasChanged(dstFile string, cmd string) bool {
	cmdFile := dstFile + ".cmd"
	prevCmd, err := ioutil.ReadFile(cmdFile)
	if err != nil {
		return true
	}

	return bytes.Compare(prevCmd, []byte(cmd)) != 0
}

// Determines if the specified C or assembly file needs to be built.  A compile
// is required if any of the following is true:
//     * The destination object file does not exist.
//     * The existing object file was built with a different compiler
//       invocation.
//     * The source file has a newer modification time than the object file.
//     * One or more included header files has a newer modification time than
//       the object file.
func (tracker *DepTracker) CompileRequired(srcFile string,
	compilerType int) (bool, error) {

	objFile := tracker.compiler.DstDir() + "/" +
		strings.TrimSuffix(srcFile, filepath.Ext(srcFile)) + ".o"
	depFile := tracker.compiler.DstDir() + "/" +
		strings.TrimSuffix(srcFile, filepath.Ext(srcFile)) + ".d"

	// If the object was previously built with a different set of options, a
	// rebuild is necessary.
	cmd, err := tracker.compiler.CompileFileCmd(srcFile, compilerType)
	if err != nil {
		return false, err
	}

	if commandHasChanged(objFile, cmd) {
		util.StatusMessage(util.VERBOSITY_VERBOSE, "%s - rebuild required; "+
			"different command\n", srcFile)
		err := tracker.compiler.GenDepsForFile(srcFile)
		if err != nil {
			return false, err
		}
		return true, nil
	}

	if util.NodeNotExist(depFile) {
		err := tracker.compiler.GenDepsForFile(srcFile)
		if err != nil {
			return false, err
		}
	}

	srcModTime, err := util.FileModificationTime(srcFile)
	if err != nil {
		return false, err
	}

	objModTime, err := util.FileModificationTime(objFile)
	if err != nil {
		return false, err
	}

	// If the object doesn't exist or is older than the source file, a build is
	// required; no need to check dependencies.
	if srcModTime.After(objModTime) {
		util.StatusMessage(util.VERBOSITY_VERBOSE, "%s - rebuild required; "+
			"source newer than obj\n", srcFile)
		return true, nil
	}

	// Determine if the dependency (.d) file needs to be generated.  If it
	// doesn't exist or is older than the source file, it is out of date and
	// needs to be created.
	depModTime, err := util.FileModificationTime(depFile)
	if err != nil {
		return false, err
	}

	if srcModTime.After(depModTime) {
		err := tracker.compiler.GenDepsForFile(srcFile)
		if err != nil {
			return false, err
		}
	}

	// Extract the dependency filenames from the dependency file.
	deps, err := ParseDepsFile(depFile)
	if err != nil {
		return false, err
	}

	// Check if any dependencies are newer than the destination object file.
	for _, dep := range deps {
		if util.NodeNotExist(dep) {
			// The dependency has been deleted; a rebuild is required.  Also,
			// the dependency file is out of date, so it needs to be deleted.
			// We cannot regenerate it now because the source file might be
			// including a nonexistent header.
			os.Remove(depFile)
			return true, nil
		} else {
			depModTime, err = util.FileModificationTime(dep)
			if err != nil {
				return false, err
			}
		}

		if depModTime.After(objModTime) {
			util.StatusMessage(util.VERBOSITY_VERBOSE, "%s - rebuild required; obj older than dependency (%s)\n", srcFile, dep)
			return true, nil
		}
	}

	return false, nil
}

// Determines if the specified static library needs to be rearchived.  The
// library needs to be archived if any of the following is true:
//     * The destination library file does not exist.
//     * The existing library file was built with a different compiler
//       invocation.
//     * One or more source object files has a newer modification time than the
//       library file.
func (tracker *DepTracker) ArchiveRequired(archiveFile string,
	objFiles []string) (bool, error) {

	// If the archive was previously built with a different set of options, a
	// rebuild is required.
	cmd := tracker.compiler.CompileArchiveCmd(archiveFile, objFiles)
	if commandHasChanged(archiveFile, cmd) {
		return true, nil
	}

	// If the archive doesn't exist or is older than any object file, a rebuild
	// is required.
	aModTime, err := util.FileModificationTime(archiveFile)
	if err != nil {
		return false, err
	}
	if tracker.MostRecent.After(aModTime) {
		return true, nil
	}

	// The library is up to date.
	return false, nil
}

func (tracker *DepTracker) TrimmedArchiveRequired(dstFile string,
	srcFile string, elfLib string) (bool, error) {

	// If the .A file doesn't exist or is older than the input file, a rebuild
	// is required.
	dstModTime, err := util.FileModificationTime(dstFile)
	if err != nil {
		return false, err
	}

	// If the elf file doesn't exist or is older than any input file,
	// a rebuild is required.
	if elfLib != "" {
		elfDstModTime, err := util.FileModificationTime(elfLib)
		if err != nil {
			return false, err
		}

		if elfDstModTime.After(dstModTime) {
			return true, nil
		}
	}
	objModTime, err := util.FileModificationTime(srcFile)
	if err != nil {
		return false, err
	}

	if objModTime.After(dstModTime) {
		return true, nil
	}
	return false, nil
}

// Determines if the specified elf file needs to be linked.  Linking is
// necessary if the elf file does not exist or has an older modification time
// than any source object or library file.
// Determines if the specified static library needs to be rearchived.  The
// library needs to be archived if any of the following is true:
//     * The destination library file does not exist.
//     * The existing library file was built with a different compiler
//       invocation.
//     * One or more source object files has a newer modification time than the
//       library file.
func (tracker *DepTracker) LinkRequired(dstFile string,
	options map[string]bool, objFiles []string,
	keepSymbols []string, elfLib string) (bool, error) {

	// If the elf file was previously built with a different set of options, a
	// rebuild is required.
	cmd := tracker.compiler.CompileBinaryCmd(dstFile, options, objFiles, keepSymbols, elfLib)
	if commandHasChanged(dstFile, cmd) {
		util.StatusMessage(util.VERBOSITY_VERBOSE, "%s - link required; "+
			"different command\n", dstFile)
		return true, nil
	}

	// If the elf file doesn't exist or is older than any input file, a rebuild
	// is required.
	dstModTime, err := util.FileModificationTime(dstFile)
	if err != nil {
		return false, err
	}

	// If the elf file doesn't exist or is older than any input file, a rebuild
	// is required.
	if elfLib != "" {
		elfDstModTime, err := util.FileModificationTime(elfLib)
		if err != nil {
			return false, err
		}
		if elfDstModTime.After(dstModTime) {
			util.StatusMessage(util.VERBOSITY_VERBOSE, "%s - link required; "+
				"old elf file\n", elfLib)
			return true, nil
		}
	}

	// Check timestamp of each .o file in the project.
	if tracker.MostRecent.After(dstModTime) {
		util.StatusMessage(util.VERBOSITY_VERBOSE, "%s - link required; "+
			"source newer than elf\n", dstFile)
		return true, nil
	}

	// Check timestamp of the linker script and all input libraries.
	for _, ls := range tracker.compiler.LinkerScripts {
		objFiles = append(objFiles, ls)
	}
	for _, obj := range objFiles {
		objModTime, err := util.FileModificationTime(obj)
		if err != nil {
			return false, err
		}

		if objModTime.After(dstModTime) {
			util.StatusMessage(util.VERBOSITY_VERBOSE, "%s - rebuild "+
				"required; obj older than dependency (%s)\n", dstFile, obj)
			return true, nil
		}
	}

	return false, nil
}

/* Building a ROM elf is used for shared application linking.
 * A ROM elf requires a rebuild if any of archives (.a files) are newer
 * than the rom elf, or if the elf file is newer than the rom_elf */
func (tracker *DepTracker) RomElfBuildRequired(dstFile string, elfFile string,
	archFiles []string) (bool, error) {

	// If the rom_elf file doesn't exist or is older than any input file, a
	// rebuild is required.
	dstModTime, err := util.FileModificationTime(dstFile)
	if err != nil {
		return false, err
	}

	// If the elf file doesn't exist or is older than any input file, a rebuild
	// is required.
	elfDstModTime, err := util.FileModificationTime(elfFile)
	if err != nil {
		return false, err
	}

	if elfDstModTime.After(dstModTime) {
		return true, nil
	}

	for _, arch := range archFiles {
		objModTime, err := util.FileModificationTime(arch)
		if err != nil {
			return false, err
		}

		if objModTime.After(dstModTime) {
			return true, nil
		}
	}
	return false, nil
}

// Determines if the specified static library needs to be copied.  The
// library needs to be archived if any of the following is true:
//     * The destination library file does not exist.
//     * Source object files has a newer modification time than the
//       target file.
func (tracker *DepTracker) CopyRequired(srcFile string) (bool, error) {

	tgtFile := tracker.compiler.DstDir() + "/" + filepath.Base(srcFile)

	// If the target doesn't exist or is older than source file, a copy
	// is required.
	srcModTime, err := util.FileModificationTime(srcFile)
	if err != nil {
		return false, err
	}
	tgtModTime, err := util.FileModificationTime(tgtFile)
	if err != nil {
		return false, err
	}
	if srcModTime.After(tgtModTime) {
		return true, nil
	}

	// The target is up to date.
	return false, nil
}
