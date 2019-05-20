// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This specific file handles the CLI commands that interact with objects in the cluster
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/NVIDIA/aistore/cli/templates"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/urfave/cli"
)

const (
	objGet      = "get"
	objPut      = "put"
	objDel      = "delete"
	objStat     = "stat"
	objPrefetch = cmn.ActPrefetch
	objEvict    = commandEvict
)

var (
	nameFlag     = cli.StringFlag{Name: "name", Usage: "name of object"}
	outFileFlag  = cli.StringFlag{Name: "out-file", Usage: "name of the file where the contents will be saved"}
	fileFlag     = cli.StringFlag{Name: "file", Usage: "filepath for content of the object"}
	newNameFlag  = cli.StringFlag{Name: "new-name", Usage: "new name of object"}
	offsetFlag   = cli.StringFlag{Name: cmn.URLParamOffset, Usage: "object read offset"}
	lengthFlag   = cli.StringFlag{Name: cmn.URLParamLength, Usage: "object read length"}
	prefixFlag   = cli.StringFlag{Name: cmn.URLParamPrefix, Usage: "prefix for string matching"}
	listFlag     = cli.StringFlag{Name: "list", Usage: "comma separated list of object names, eg. 'o1,o2,o3'"}
	rangeFlag    = cli.StringFlag{Name: "range", Usage: "colon separated interval of object indices, eg. <START>:<STOP>"}
	deadlineFlag = cli.StringFlag{Name: "deadline", Usage: "amount of time (Go Duration string) before the request expires", Value: "0s"}
	cachedFlag   = cli.BoolFlag{Name: "cached", Usage: "check if an object is cached"}

	baseObjectFlags = []cli.Flag{
		bucketFlag,
		nameFlag,
		bckProviderFlag,
	}

	baseLstRngFlags = []cli.Flag{
		listFlag,
		rangeFlag,
		prefixFlag,
		regexFlag,
		waitFlag,
		deadlineFlag,
	}

	objectFlags = map[string][]cli.Flag{
		objPut: append(
			[]cli.Flag{fileFlag},
			baseObjectFlags...),
		objGet: append(
			[]cli.Flag{
				outFileFlag,
				offsetFlag,
				lengthFlag,
				checksumFlag,
				propsFlag,
				cachedFlag,
			},
			baseObjectFlags...),
		commandRename: []cli.Flag{
			bucketFlag,
			newNameFlag,
			nameFlag,
		},
		objDel: append(
			baseLstRngFlags,
			baseObjectFlags...),
		objStat: append(baseObjectFlags, jsonFlag),
		objPrefetch: append(
			[]cli.Flag{bucketFlag},
			baseLstRngFlags...),
		objEvict: append(
			[]cli.Flag{bucketFlag, nameFlag},
			baseLstRngFlags...),
	}

	objectBasicUsageText = "%s object %s --bucket <value> --name <value>"
	objectGetUsage       = fmt.Sprintf(objectBasicUsageText, cliName, objGet)
	objectDelUsage       = fmt.Sprintf(objectBasicUsageText, cliName, objDel)
	objectStatUsage      = fmt.Sprintf(objectBasicUsageText, cliName, objStat)
	objectPutUsage       = fmt.Sprintf("%s object %s --bucket <value> --name <value> --file <value>", cliName, objPut)
	objectRenameUsage    = fmt.Sprintf("%s object %s --bucket <value> --name <value> --new-name <value> ", cliName, commandRename)
	objectPrefetchUsage  = fmt.Sprintf("%s object %s [--list <value>] [--range <value> --prefix <value> --regex <value>]", cliName, objPrefetch)
	objectEvictUsage     = fmt.Sprintf("%s object %s [--list <value>] [--range <value> --prefix <value> --regex <value>]", cliName, objEvict)

	objectCmds = []cli.Command{
		{
			Name:  "object",
			Usage: "interact with objects",
			Flags: baseObjectFlags,
			Subcommands: []cli.Command{
				{
					Name:         objGet,
					Usage:        "gets the object from the specified bucket",
					UsageText:    objectGetUsage,
					Flags:        objectFlags[objGet],
					Action:       objectHandler,
					BashComplete: flagList,
				},
				{
					Name:         objPut,
					Usage:        "puts the object to the specified bucket",
					UsageText:    objectPutUsage,
					Flags:        objectFlags[objPut],
					Action:       objectHandler,
					BashComplete: flagList,
				},
				{
					Name:         objDel,
					Usage:        "deletes the object from the specified bucket",
					UsageText:    objectDelUsage,
					Flags:        objectFlags[objDel],
					Action:       objectHandler,
					BashComplete: flagList,
				},
				{
					Name:         objStat,
					Usage:        "displays basic information about the object",
					UsageText:    objectStatUsage,
					Flags:        objectFlags[objStat],
					Action:       objectHandler,
					BashComplete: flagList,
				},
				{
					Name:         commandRename,
					Usage:        "renames the local object",
					UsageText:    objectRenameUsage,
					Flags:        objectFlags[commandRename],
					Action:       objectHandler,
					BashComplete: flagList,
				},
				{
					Name:         objPrefetch,
					Usage:        "prefetches the object from the specified bucket",
					UsageText:    objectPrefetchUsage,
					Flags:        objectFlags[objPrefetch],
					Action:       objectHandler,
					BashComplete: flagList,
				},
				{
					Name:         objEvict,
					Usage:        "evicts the object from the specified bucket",
					UsageText:    objectEvictUsage,
					Flags:        objectFlags[objEvict],
					Action:       objectHandler,
					BashComplete: flagList,
				},
			},
		},
	}
)

func objectHandler(c *cli.Context) (err error) {
	if err = checkFlags(c, bucketFlag); err != nil {
		return
	}

	baseParams := cliAPIParams(ClusterURL)
	bucket := parseStrFlag(c, bucketFlag)
	bckProvider, err := cmn.BckProviderFromStr(parseStrFlag(c, bckProviderFlag))
	if err != nil {
		return
	}
	if err = canReachBucket(baseParams, bucket, bckProvider); err != nil {
		return
	}

	commandName := c.Command.Name
	switch commandName {
	case objGet:
		err = objectRetrieve(c, baseParams, bucket, bckProvider)
	case objPut:
		err = objectPut(c, baseParams, bucket, bckProvider)
	case objDel:
		err = objectDelete(c, baseParams, bucket, bckProvider)
	case objStat:
		err = objectStat(c, baseParams, bucket, bckProvider)
	case commandRename:
		err = objectRename(c, baseParams, bucket)
	case objPrefetch:
		err = objectPrefetch(c, baseParams, bucket, bckProvider)
	case objEvict:
		err = objectEvict(c, baseParams, bucket, bckProvider)
	default:
		return fmt.Errorf(invalidCmdMsg, commandName)
	}
	return err
}

// Get object from bucket
func objectRetrieve(c *cli.Context, baseParams *api.BaseParams, bucket, bckProvider string) (err error) {
	if err = checkFlags(c, nameFlag); err != nil {
		return
	}

	obj := parseStrFlag(c, nameFlag)
	offset, err := getByteFlagValue(c, offsetFlag)
	if err != nil {
		return err
	}
	length, err := getByteFlagValue(c, lengthFlag)
	if err != nil {
		return err
	}

	query := url.Values{}
	query.Add(cmn.URLParamBckProvider, bckProvider)
	query.Add(cmn.URLParamOffset, offset)
	query.Add(cmn.URLParamLength, length)
	objArgs := api.GetObjectInput{Writer: os.Stdout, Query: query}

	// Output to user location
	if flagIsSet(c, outFileFlag) {
		outFile := parseStrFlag(c, outFileFlag)
		f, err := os.Create(outFile)
		if err != nil {
			return err
		}
		defer f.Close()
		objArgs = api.GetObjectInput{Writer: f, Query: query}
	}

	if flagIsSet(c, lengthFlag) != flagIsSet(c, offsetFlag) {
		return fmt.Errorf("%s and %s flags both need to be set", lengthFlag.Name, offsetFlag.Name)
	}

	if flagIsSet(c, cachedFlag) {
		_, err := api.HeadObject(baseParams, bucket, bckProvider, obj, true)
		if err != nil {
			if err.(*cmn.HTTPError).Status == http.StatusNotFound {
				fmt.Printf("Cached: %v\n", false)
				return nil
			}
			return err
		}
		fmt.Printf("Cached: %v\n", true)
		return nil
	}

	var objLen int64
	if flagIsSet(c, checksumFlag) {
		objLen, err = api.GetObjectWithValidation(baseParams, bucket, obj, objArgs)
	} else {
		objLen, err = api.GetObject(baseParams, bucket, obj, objArgs)
	}
	if err != nil {
		return
	}

	if flagIsSet(c, lengthFlag) {
		_, _ = fmt.Fprintf(os.Stderr, "\nRead %s (%d B)\n", cmn.B2S(objLen, 2), objLen)
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "%s has size %s (%d B)\n", obj, cmn.B2S(objLen, 2), objLen)
	return
}

// Put object into bucket
func objectPut(c *cli.Context, baseParams *api.BaseParams, bucket, bckProvider string) error {
	if err := checkFlags(c, fileFlag); err != nil {
		return err
	}

	source := parseStrFlag(c, fileFlag)

	objName := parseStrFlag(c, nameFlag)
	if objName == "" {
		// If name was not provided use the last element of the object's path as object's name
		objName = filepath.Base(source)
	}

	path, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	reader, err := cmn.NewFileHandle(path)
	if err != nil {
		return err
	}

	putArgs := api.PutObjectArgs{BaseParams: baseParams, Bucket: bucket, BucketProvider: bckProvider, Object: objName, Reader: reader}
	if err := api.PutObject(putArgs); err != nil {
		return err
	}

	fmt.Printf("%s put into %s bucket\n", objName, bucket)
	return nil
}

// Deletes object from bucket
func objectDelete(c *cli.Context, baseParams *api.BaseParams, bucket, bckProvider string) (err error) {
	if flagIsSet(c, listFlag) && flagIsSet(c, rangeFlag) {
		return fmt.Errorf("cannot use both %s and %s", listFlag.Name, rangeFlag.Name)
	}

	// Normal usage
	if flagIsSet(c, nameFlag) {
		obj := parseStrFlag(c, nameFlag)
		if err = api.DeleteObject(baseParams, bucket, obj, bckProvider); err != nil {
			return
		}
		fmt.Printf("%s deleted from %s bucket\n", obj, bucket)
		return
	} else if flagIsSet(c, listFlag) {
		// List Delete
		return listOp(c, baseParams, objDel, bucket, bckProvider)
	} else if flagIsSet(c, rangeFlag) {
		// Range Delete
		return rangeOp(c, baseParams, objDel, bucket, bckProvider)
	}

	return errors.New(c.Command.UsageText)
}

// Displays object properties
func objectStat(c *cli.Context, baseParams *api.BaseParams, bucket, bckProvider string) error {
	if err := checkFlags(c, nameFlag); err != nil {
		return err
	}

	name := parseStrFlag(c, nameFlag)
	props, err := api.HeadObject(baseParams, bucket, bckProvider, name)
	if err != nil {
		return handleObjHeadError(err, bucket, name)
	}

	return templates.DisplayOutput(props, templates.ObjStatTmpl, flagIsSet(c, jsonFlag))
}

// Prefetch operations
func objectPrefetch(c *cli.Context, baseParams *api.BaseParams, bucket, bckProvider string) (err error) {
	if flagIsSet(c, listFlag) {
		// List prefetch
		return listOp(c, baseParams, objPrefetch, bucket, bckProvider)
	} else if flagIsSet(c, rangeFlag) {
		// Range prefetch
		return rangeOp(c, baseParams, objPrefetch, bucket, bckProvider)
	}

	return errors.New(c.Command.UsageText)
}

// Evict operations
func objectEvict(c *cli.Context, baseParams *api.BaseParams, bucket, bckProvider string) (err error) {
	if flagIsSet(c, nameFlag) {
		// Name evict
		name := parseStrFlag(c, nameFlag)
		if err := api.EvictObject(baseParams, bucket, name); err != nil {
			return err
		}
		fmt.Printf("%s evicted from %s bucket\n", name, bucket)
		return
	} else if flagIsSet(c, listFlag) {
		// List evict
		return listOp(c, baseParams, objEvict, bucket, bckProvider)
	} else if flagIsSet(c, rangeFlag) {
		// Range evict
		return rangeOp(c, baseParams, objEvict, bucket, bckProvider)
	}

	return errors.New(c.Command.UsageText)
}

// Renames object
func objectRename(c *cli.Context, baseParams *api.BaseParams, bucket string) (err error) {
	if err = checkFlags(c, nameFlag, newNameFlag); err != nil {
		return
	}
	obj := parseStrFlag(c, nameFlag)
	newName := parseStrFlag(c, newNameFlag)
	if err = api.RenameObject(baseParams, bucket, obj, newName); err != nil {
		return
	}

	fmt.Printf("%s renamed to %s\n", obj, newName)
	return
}

// =======================HELPERS=========================
// List handler
func listOp(c *cli.Context, baseParams *api.BaseParams, command, bucket, bckProvider string) (err error) {
	fileList := makeList(parseStrFlag(c, listFlag), ",")
	wait := flagIsSet(c, waitFlag)
	deadline, err := time.ParseDuration(parseStrFlag(c, deadlineFlag))
	if err != nil {
		return
	}

	switch command {
	case objDel:
		err = api.DeleteList(baseParams, bucket, bckProvider, fileList, wait, deadline)
		command += "d"
	case objPrefetch:
		err = api.PrefetchList(baseParams, bucket, cmn.CloudBs, fileList, wait, deadline)
		command += "ed"
	case objEvict:
		err = api.EvictList(baseParams, bucket, cmn.CloudBs, fileList, wait, deadline)
		command += "ed"
	default:
		return fmt.Errorf(invalidCmdMsg, command)
	}
	if err != nil {
		return
	}
	fmt.Printf("%s %s from %s bucket\n", fileList, command, bucket)
	return
}

// Range handler
func rangeOp(c *cli.Context, baseParams *api.BaseParams, command, bucket, bckProvider string) (err error) {
	var (
		wait     = flagIsSet(c, waitFlag)
		prefix   = parseStrFlag(c, prefixFlag)
		regex    = parseStrFlag(c, regexFlag)
		rangeStr = parseStrFlag(c, rangeFlag)
	)

	deadline, err := time.ParseDuration(parseStrFlag(c, deadlineFlag))
	if err != nil {
		return
	}

	switch command {
	case objDel:
		err = api.DeleteRange(baseParams, bucket, bckProvider, prefix, regex, rangeStr, wait, deadline)
		command += "d"
	case objPrefetch:
		err = api.PrefetchRange(baseParams, bucket, cmn.CloudBs, prefix, regex, rangeStr, wait, deadline)
		command += "ed"
	case objEvict:
		err = api.EvictRange(baseParams, bucket, cmn.CloudBs, prefix, regex, rangeStr, wait, deadline)
		command += "ed"
	default:
		return fmt.Errorf(invalidCmdMsg, command)
	}
	if err != nil {
		return
	}
	fmt.Printf("%s files with prefix '%s' matching '%s' in the range '%s' from %s bucket\n",
		command, prefix, regex, rangeStr, bucket)
	return
}

// This function is needed to print a nice error message for the user
func handleObjHeadError(err error, bucket, object string) error {
	httpErr, ok := err.(*cmn.HTTPError)
	if !ok {
		return err
	}
	if httpErr.Status == http.StatusNotFound {
		return fmt.Errorf("no such object %q in bucket %q", object, bucket)
	}

	return err
}

// Returns a string containing the value of the `flag` in bytes, used for `offset` and `length` flags
func getByteFlagValue(c *cli.Context, flag cli.Flag) (string, error) {
	if flagIsSet(c, flag) {
		offsetInt, err := parseByteFlagToInt(c, flag)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(offsetInt, 10), nil
	}

	return "", nil
}
