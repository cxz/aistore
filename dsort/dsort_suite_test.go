// Package dsort provides distributed massively parallel resharding for very large datasets.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dsort_test

import (
	"fmt"
	"testing"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/hk"
	"github.com/NVIDIA/aistore/xaction"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestDSort(t *testing.T) {
	xaction.Init()
	RegisterFailHandler(Fail)
	go hk.DefaultHK.Run()
	RunSpecs(t, fmt.Sprintf("%s Suite", cmn.DSortName))
}
