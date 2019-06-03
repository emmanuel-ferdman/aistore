/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/NVIDIA/aistore/bench/aisloader/namegetter"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/tutils/tassert"
)

// Running these benchmarks with different objNamesSize returns different results
// Almost for every number of objects, permutation strategies outcompete random

const objNamesSize = 200000
const smallSampleSize = 1000

var objnames []string

func init() {
	objnames = make([]string, objNamesSize)
	for i := 0; i < objNamesSize; i++ {
		objnames[i] = fmt.Sprintf("test-%d", i)
	}
}

func BenchmarkRandomUniqueNameGetter(b *testing.B) {
	ng := &namegetter.RandomUniqueNameGetter{}
	ng.Init(objnames, cmn.NowRand())
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		ng.ObjName()
	}
}

func BenchmarkRandomUniqueIterNameGetter(b *testing.B) {
	ng := &namegetter.RandomUniqueIterNameGetter{}
	ng.Init(objnames, cmn.NowRand())
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		ng.ObjName()
	}
}

func BenchmarkPermutationUniqueNameGetter(b *testing.B) {
	ng := &namegetter.PermutationUniqueNameGetter{}
	ng.Init(objnames, cmn.NowRand())
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		ng.ObjName()
	}
}

func BenchmarkPermutationImprovedUniqueNameGetter(b *testing.B) {
	ng := &namegetter.PermutationUniqueImprovedNameGetter{}
	ng.Init(objnames, cmn.NowRand())
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		ng.ObjName()
	}
}

func TestRandomUniqueNameGetter(t *testing.T) {
	ng := &namegetter.RandomUniqueNameGetter{}

	checkGetsAllObjNames(t, ng, "RandomUniqueNameGetter")
	checkSmallSampleRandomness(t, ng, "RandomUniqueNameGetter")
}

func TestRandomUniqueIterNameGetter(t *testing.T) {
	ng := &namegetter.RandomUniqueIterNameGetter{}

	checkGetsAllObjNames(t, ng, "RandomUniqueIterNameGetter")
	checkSmallSampleRandomness(t, ng, "RandomUniqueIterNameGetter")
}

func TestPermutationUniqueNameGetter(t *testing.T) {
	ng := &namegetter.PermutationUniqueNameGetter{}

	checkGetsAllObjNames(t, ng, "PermutationUniqueNameGetter")
	checkSmallSampleRandomness(t, ng, "PermutationUniqueNameGetter")
}

func TestPermutationUniqueImprovedNameGetter(t *testing.T) {
	ng := &namegetter.PermutationUniqueImprovedNameGetter{}

	checkGetsAllObjNames(t, ng, "PermutationUniqueImprovedNameGetter")
	checkSmallSampleRandomness(t, ng, "PermutationUniqueImprovedNameGetter")
}

func checkGetsAllObjNames(t *testing.T, getter namegetter.ObjectNameGetter, name string) {
	getter.Init(objnames, cmn.NowRand())
	m := make(map[string]struct{})

	// Should visit every objectName once
	for i := 0; i < objNamesSize; i++ {
		m[getter.ObjName()] = struct{}{}
	}

	tassert.Fatalf(t, len(m) == objNamesSize, "%s has not visited every element; got %d, expected %d", name, len(m), objNamesSize)

	// Check that starting operation for the beginning still works as expected
	m = make(map[string]struct{})
	for i := 0; i < objNamesSize; i++ {
		m[getter.ObjName()] = struct{}{}
	}
	tassert.Fatalf(t, len(m) == objNamesSize, "%s has not visited every element for second time; got %d, expected %d", name, len(m), objNamesSize)
}

func checkSmallSampleRandomness(t *testing.T, getter namegetter.ObjectNameGetter, name string) {
	s1 := make([]string, smallSampleSize)
	s2 := make([]string, smallSampleSize)

	rnd := cmn.NowRand()

	getter.Init(objnames, rnd)
	for i := 0; i < smallSampleSize; i++ {
		s1[i] = getter.ObjName()
	}
	getter.Init(objnames, rnd)
	for i := 0; i < smallSampleSize; i++ {
		s2[i] = getter.ObjName()
	}

	tassert.Fatalf(t, !reflect.DeepEqual(s1, s2), name+" is not random!")
}
