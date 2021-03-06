// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package plan

import (
	"math"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/parser/opcode"
	"github.com/pingcap/tidb/util/types"
)

// Refine tries to build index or table range.
func Refine(p Plan) error {
	return refine(p)
}

func refine(in Plan) error {
	for _, c := range in.GetChildren() {
		e := refine(c)
		if e != nil {
			return errors.Trace(e)
		}
	}

	var err error
	switch x := in.(type) {
	case *IndexScan:
		err = buildIndexRange(x)
	case *Limit:
		x.SetLimit(0)
	case *TableScan:
		err = buildTableRange(x)
	case *NewTableScan:
		x.Ranges = []TableRange{{math.MinInt64, math.MaxInt64}}
	case *Selection:
		err = buildSelection(x)
	}
	return errors.Trace(err)
}

var fullRange = []rangePoint{
	{start: true},
	{value: types.MaxValueDatum()},
}

func buildIndexRange(p *IndexScan) error {
	rb := rangeBuilder{}
	if p.AccessEqualCount > 0 {
		// Build ranges for equal access conditions.
		point := rb.build(p.AccessConditions[0])
		p.Ranges = rb.buildIndexRanges(point)
		for i := 1; i < p.AccessEqualCount; i++ {
			point = rb.build(p.AccessConditions[i])
			p.Ranges = rb.appendIndexRanges(p.Ranges, point)
		}
	}
	rangePoints := fullRange
	// Build rangePoints for non-equal access condtions.
	for i := p.AccessEqualCount; i < len(p.AccessConditions); i++ {
		rangePoints = rb.intersection(rangePoints, rb.build(p.AccessConditions[i]))
	}
	if p.AccessEqualCount == 0 {
		p.Ranges = rb.buildIndexRanges(rangePoints)
	} else if p.AccessEqualCount < len(p.AccessConditions) {
		p.Ranges = rb.appendIndexRanges(p.Ranges, rangePoints)
	}
	return errors.Trace(rb.err)
}

func buildTableRange(p *TableScan) error {
	if len(p.AccessConditions) == 0 {
		p.Ranges = []TableRange{{math.MinInt64, math.MaxInt64}}
		return nil
	}
	rb := rangeBuilder{}
	rangePoints := fullRange
	for _, cond := range p.AccessConditions {
		rangePoints = rb.intersection(rangePoints, rb.build(cond))
	}
	p.Ranges = rb.buildTableRanges(rangePoints)
	return errors.Trace(rb.err)
}

func buildSelection(p *Selection) error {
	var err error
	var accessConditions []expression.Expression
	switch p.GetChildByIndex(0).(type) {
	case *NewTableScan:
		tableScan := p.GetChildByIndex(0).(*NewTableScan)
		accessConditions, p.Conditions = detachConditions(p.Conditions, tableScan.Table, nil, 0)
		err = buildNewTableRange(tableScan, accessConditions)
		// TODO: Implement NewIndexScan
	}

	return errors.Trace(err)
}

// detachConditions distinguishs between access conditions and filter conditions from conditions.
func detachConditions(conditions []expression.Expression, table *model.TableInfo,
	idx *model.IndexInfo, colOffset int) ([]expression.Expression, []expression.Expression) {
	var pkName model.CIStr
	var accessConditions, filterConditions []expression.Expression
	if table.PKIsHandle {
		for _, colInfo := range table.Columns {
			if mysql.HasPriKeyFlag(colInfo.Flag) {
				pkName = colInfo.Name
				break
			}
		}
	}
	for _, con := range conditions {
		if pkName.L != "" {
			checker := conditionChecker{
				tableName:    table.Name,
				pkName:       pkName,
				idx:          idx,
				columnOffset: colOffset}
			if checker.newCheck(con) {
				accessConditions = append(accessConditions, con)
				continue
			}
		}
		filterConditions = append(filterConditions, con)
	}

	return accessConditions, filterConditions
}

func buildNewTableRange(p *NewTableScan, accessConditions []expression.Expression) error {
	if len(accessConditions) == 0 {
		p.Ranges = []TableRange{{math.MinInt64, math.MaxInt64}}
		return nil
	}

	p.AccessCondition = accessConditions
	rb := rangeBuilder{}
	rangePoints := fullRange
	for _, cond := range accessConditions {
		rangePoints = rb.intersection(rangePoints, rb.newBuild(cond))
		if rb.err != nil {
			return errors.Trace(rb.err)
		}
	}
	p.Ranges = rb.buildTableRanges(rangePoints)
	return errors.Trace(rb.err)
}

// conditionChecker checks if this condition can be pushed to index plan.
type conditionChecker struct {
	tableName model.CIStr
	idx       *model.IndexInfo
	// the offset of the indexed column to be checked.
	columnOffset int
	pkName       model.CIStr
}

func (c *conditionChecker) check(condition ast.ExprNode) bool {
	switch x := condition.(type) {
	case *ast.BinaryOperationExpr:
		return c.checkBinaryOperation(x)
	case *ast.BetweenExpr:
		if ast.IsPreEvaluable(x.Left) && ast.IsPreEvaluable(x.Right) && c.checkColumnExpr(x.Expr) {
			return true
		}
	case *ast.ColumnNameExpr:
		return c.checkColumnExpr(x)
	case *ast.IsNullExpr:
		if c.checkColumnExpr(x.Expr) {
			return true
		}
	case *ast.IsTruthExpr:
		if c.checkColumnExpr(x.Expr) {
			return true
		}
	case *ast.ParenthesesExpr:
		return c.check(x.Expr)
	case *ast.PatternInExpr:
		if x.Sel != nil || x.Not {
			return false
		}
		if !c.checkColumnExpr(x.Expr) {
			return false
		}
		for _, val := range x.List {
			if !ast.IsPreEvaluable(val) {
				return false
			}
		}
		return true
	case *ast.PatternLikeExpr:
		if x.Not {
			return false
		}
		if !c.checkColumnExpr(x.Expr) {
			return false
		}
		if !ast.IsPreEvaluable(x.Pattern) {
			return false
		}
		patternVal := x.Pattern.GetValue()
		if patternVal == nil {
			return false
		}
		patternStr, err := types.ToString(patternVal)
		if err != nil {
			return false
		}
		firstChar := patternStr[0]
		return firstChar != '%' && firstChar != '.'
	}
	return false
}

func (c *conditionChecker) checkBinaryOperation(b *ast.BinaryOperationExpr) bool {
	switch b.Op {
	case opcode.OrOr:
		return c.check(b.L) && c.check(b.R)
	case opcode.AndAnd:
		return c.check(b.L) && c.check(b.R)
	case opcode.EQ, opcode.NE, opcode.GE, opcode.GT, opcode.LE, opcode.LT:
		if ast.IsPreEvaluable(b.L) {
			return c.checkColumnExpr(b.R)
		} else if ast.IsPreEvaluable(b.R) {
			return c.checkColumnExpr(b.L)
		}
	}
	return false
}

func (c *conditionChecker) checkColumnExpr(expr ast.ExprNode) bool {
	cn, ok := expr.(*ast.ColumnNameExpr)
	if !ok {
		return false
	}
	if cn.Refer.Table.Name.L != c.tableName.L {
		return false
	}
	if c.pkName.L != "" {
		return c.pkName.L == cn.Refer.Column.Name.L
	}
	if c.idx != nil {
		return cn.Refer.Column.Name.L == c.idx.Columns[c.columnOffset].Name.L
	}
	return true
}

func (c *conditionChecker) newCheck(condition expression.Expression) bool {
	switch x := condition.(type) {
	case *expression.ScalarFunction:
		return c.checkScalarFunction(x)
	case *expression.Column:
		return c.checkColumn(x)
	case *expression.Constant:
		return true
	}
	return false
}

func (c *conditionChecker) checkScalarFunction(scalar *expression.ScalarFunction) bool {
	// TODO: Implement parentheses, patternin and patternlike.
	// Expression needs to implement IsPreEvaluable function.
	switch scalar.FuncName.L {
	case ast.OrOr, ast.AndAnd:
		return c.newCheck(scalar.Args[0]) && c.newCheck(scalar.Args[1])
	case ast.EQ, ast.NE, ast.GE, ast.GT, ast.LE, ast.LT:
		if _, ok := scalar.Args[0].(*expression.Constant); ok {
			return c.checkColumn(scalar.Args[1])
		}
		if _, ok := scalar.Args[1].(*expression.Constant); ok {
			return c.checkColumn(scalar.Args[0])
		}
	case ast.IsNull, ast.IsTruth, ast.IsFalsity:
		return c.checkColumn(scalar.Args[0])
	case ast.UnaryNot:
		return c.newCheck(scalar.Args[0])
	}
	return false
}

func (c *conditionChecker) checkColumn(expr expression.Expression) bool {
	col, ok := expr.(*expression.Column)
	if !ok {
		return false
	}
	if col.TblName.L != c.tableName.L {
		return false
	}
	if c.pkName.L != "" {
		return c.pkName.L == col.ColName.L
	}
	if c.idx != nil {
		return col.ColName.L == c.idx.Columns[c.columnOffset].Name.L
	}
	return true
}
