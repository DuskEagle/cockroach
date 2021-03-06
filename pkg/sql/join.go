// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package sql

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
)

// joinNode is a planNode whose rows are the result of an inner or
// left/right outer join.
type joinNode struct {
	joinType sqlbase.JoinType

	// The data sources.
	left  planDataSource
	right planDataSource

	// pred represents the join predicate.
	pred *joinPredicate

	// mergeJoinOrdering is set if the left and right sides have similar ordering
	// on the equality columns (or a subset of them). The column indices refer to
	// equality columns: a ColIdx of i refers to left column
	// pred.leftEqualityIndices[i] and right column pred.rightEqualityIndices[i].
	// See computeMergeJoinOrdering. This information is used by distsql planning.
	mergeJoinOrdering sqlbase.ColumnOrdering

	props physicalProps

	// columns contains the metadata for the results of this node.
	columns sqlbase.ResultColumns
}

// makeJoinPredicate builds a joinPredicate from a join condition. Also returns
// any USING or NATURAL JOIN columns (these need to be merged into one column
// after the join).
func (p *planner) makeJoinPredicate(
	ctx context.Context,
	left *sqlbase.DataSourceInfo,
	right *sqlbase.DataSourceInfo,
	joinType sqlbase.JoinType,
	cond tree.JoinCond,
) (*joinPredicate, []usingColumn, error) {
	switch cond.(type) {
	case tree.NaturalJoinCond, *tree.UsingJoinCond:
		var usingColNames tree.NameList

		switch t := cond.(type) {
		case tree.NaturalJoinCond:
			usingColNames = commonColumns(left, right)
		case *tree.UsingJoinCond:
			usingColNames = t.Cols
		}

		usingColumns, err := makeUsingColumns(
			left.SourceColumns, right.SourceColumns, usingColNames,
		)
		if err != nil {
			return nil, nil, err
		}
		pred, err := makePredicate(joinType, left, right, usingColumns)
		if err != nil {
			return nil, nil, err
		}
		return pred, usingColumns, nil

	case nil, *tree.OnJoinCond:
		pred, err := makePredicate(joinType, left, right, nil /* usingColumns */)
		if err != nil {
			return nil, nil, err
		}
		switch t := cond.(type) {
		case *tree.OnJoinCond:
			// Do not allow special functions in the ON clause.
			p.semaCtx.Properties.Require("ON", tree.RejectSpecial)

			// Determine the on condition expression. Note that the predicate can't
			// already have onCond set (we haven't passed any usingColumns).
			pred.onCond, err = p.analyzeExpr(
				ctx,
				t.Expr,
				sqlbase.MultiSourceInfo{pred.info},
				pred.iVarHelper,
				types.Bool,
				true, /* requireType */
				"ON",
			)
			if err != nil {
				return nil, nil, err
			}
		}
		return pred, nil /* usingColumns */, nil

	default:
		panic(fmt.Sprintf("unsupported join condition %#v", cond))
	}
}

func (p *planner) makeJoinNode(
	left planDataSource, right planDataSource, pred *joinPredicate,
) *joinNode {
	n := &joinNode{
		left:     left,
		right:    right,
		joinType: pred.joinType,
		pred:     pred,
		columns:  pred.info.SourceColumns,
	}
	return n
}

func (n *joinNode) startExec(params runParams) error {
	panic("joinNode cannot be run in local mode")
}

// Next implements the planNode interface.
func (n *joinNode) Next(params runParams) (res bool, err error) {
	panic("joinNode cannot be run in local mode")
}

// Values implements the planNode interface.
func (n *joinNode) Values() tree.Datums {
	panic("joinNode cannot be run in local mode")
}

// Close implements the planNode interface.
func (n *joinNode) Close(ctx context.Context) {
	n.right.plan.Close(ctx)
	n.left.plan.Close(ctx)
}

// commonColumns returns the names of columns common on the
// right and left sides, for use by NATURAL JOIN.
func commonColumns(left, right *sqlbase.DataSourceInfo) tree.NameList {
	var res tree.NameList
	for _, cLeft := range left.SourceColumns {
		if cLeft.Hidden {
			continue
		}
		for _, cRight := range right.SourceColumns {
			if cRight.Hidden {
				continue
			}

			if cLeft.Name == cRight.Name {
				res = append(res, tree.Name(cLeft.Name))
			}
		}
	}
	return res
}

// interleavedNodes returns the ancestor on which an interleaved join is
// defined as well as the descendants of this ancestor which participate in
// the join. One of the left/right scan nodes is the ancestor and the other
// descendant. Nils are returned if there is no interleaved relationship.
// TODO(richardwu): For sibling joins, both left and right tables are
// "descendants" while the ancestor is some common ancestor. We will need to
// probably return descendants as a slice.
//
// An interleaved join has an equality on some columns of the interleave prefix.
// The "interleaved join ancestor" is the ancestor which contains all these
// join columns in its primary key.
// TODO(richardwu): For prefix/subset joins, this ancestor will be the furthest
// ancestor down the interleaved hierarchy which contains all the columns of
// the maximal join prefix (see maximalJoinPrefix in distsql_join.go).
func (n *joinNode) interleavedNodes() (ancestor *scanNode, descendant *scanNode) {
	leftScan, leftOk := n.left.plan.(*scanNode)
	rightScan, rightOk := n.right.plan.(*scanNode)

	if !leftOk || !rightOk {
		return nil, nil
	}

	leftAncestors := leftScan.index.Interleave.Ancestors
	rightAncestors := rightScan.index.Interleave.Ancestors

	// A join between an ancestor and descendant: the descendant of the two
	// tables must have have more interleaved ancestors than the other,
	// which makes the other node the potential interleaved ancestor.
	// TODO(richardwu): The general case where we can have a join
	// on a common ancestor's primary key requires traversing both
	// ancestor slices.
	if len(leftAncestors) > len(rightAncestors) {
		ancestor = rightScan
		descendant = leftScan
	} else {
		ancestor = leftScan
		descendant = rightScan
	}

	// We check the ancestors of the potential descendant to see if any of
	// them match the potential ancestor.
	for _, descAncestor := range descendant.index.Interleave.Ancestors {
		if descAncestor.TableID == ancestor.desc.ID && descAncestor.IndexID == ancestor.index.ID {
			// If the tables are indeed interleaved, then we return
			// the potentials as confirmed ancestor-descendant.
			return ancestor, descendant
		}
	}

	// We could not establish an ancestor-descendant relationship so we
	// return nils for both.
	return nil, nil
}
