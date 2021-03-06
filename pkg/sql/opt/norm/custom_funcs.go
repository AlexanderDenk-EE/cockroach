// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package norm

import (
	"fmt"
	"math"
	"reflect"

	"github.com/cockroachdb/apd"
	"github.com/cockroachdb/cockroach/pkg/sql/coltypes"
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/xfunc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"
	"github.com/cockroachdb/cockroach/pkg/util/json"
)

// CustomFuncs contains all the custom match and replace functions used by
// the normalization rules. The unnamed xfunc.CustomFuncs allows
// CustomFuncs to provide a clean interface for calling functions from both the
// norm and xfunc packages using the same struct.
type CustomFuncs struct {
	xfunc.CustomFuncs
	f *Factory
}

// MakeCustomFuncs returns a new CustomFuncs initialized with the given factory.
func MakeCustomFuncs(f *Factory) CustomFuncs {
	return CustomFuncs{
		CustomFuncs: xfunc.MakeCustomFuncs(f.mem, f.evalCtx),
		f:           f,
	}
}

// ----------------------------------------------------------------------
//
// List functions
//   General custom match and replace functions used to test and construct
//   lists.
//
// ----------------------------------------------------------------------

// ListOnlyHasNulls if every item in the given list is a Null op. If the list
// is empty, ListOnlyHasNulls returns false.
func (c *CustomFuncs) ListOnlyHasNulls(list memo.ListID) bool {
	if list.Length == 0 {
		return false
	}

	for _, item := range c.f.mem.LookupList(list) {
		if c.f.mem.NormExpr(item).Operator() != opt.NullOp {
			return false
		}
	}
	return true
}

// RemoveListItem returns a new list that is a copy of the given list, except
// that it does not contain the given search item. If the list contains the item
// multiple times, then only the first instance is removed. If the list does not
// contain the item, then the method is a no-op.
func (c *CustomFuncs) RemoveListItem(list memo.ListID, search memo.GroupID) memo.ListID {
	existingList := c.f.mem.LookupList(list)
	b := xfunc.MakeListBuilder(&c.CustomFuncs)
	for i, item := range existingList {
		if item == search {
			b.AddItems(existingList[i+1:])
			break
		}
		b.AddItem(item)
	}
	return b.BuildList()
}

// ReplaceListItem returns a new list that is a copy of the given list, except
// that the given search item has been replaced by the given replace item. If
// the list contains the search item multiple times, then only the first
// instance is replaced. If the list does not contain the item, then the method
// is a no-op.
func (c *CustomFuncs) ReplaceListItem(list memo.ListID, search, replace memo.GroupID) memo.ListID {
	existingList := c.f.mem.LookupList(list)
	b := xfunc.MakeListBuilder(&c.CustomFuncs)

	for i, item := range existingList {
		if item == search {
			// Replace item and copy remainder of list.
			b.AddItem(replace)
			b.AddItems(existingList[i+1:])
			break
		}
		b.AddItem(item)
	}
	return b.BuildList()
}

// ----------------------------------------------------------------------
//
// Typing functions
//   General custom match and replace functions used to test and construct
//   expression data types.
//
// ----------------------------------------------------------------------

// HasColType returns true if the given expression has a static type that's
// equivalent to the requested coltype.
func (c *CustomFuncs) HasColType(group memo.GroupID, colTyp memo.PrivateID) bool {
	srcTyp, _ := coltypes.DatumTypeToColumnType(c.LookupScalar(group).Type)
	dstTyp := c.f.mem.LookupPrivate(colTyp).(coltypes.T)
	if reflect.TypeOf(srcTyp) != reflect.TypeOf(dstTyp) {
		return false
	}
	return coltypes.ColTypeAsString(srcTyp) == coltypes.ColTypeAsString(dstTyp)
}

// IsString returns true if the given expression is of type String.
func (c *CustomFuncs) IsString(group memo.GroupID) bool {
	return c.LookupScalar(group).Type == types.String
}

// ColTypeToDatumType maps the given column type to a datum type.
func (c *CustomFuncs) ColTypeToDatumType(colTyp memo.PrivateID) memo.PrivateID {
	datumTyp := coltypes.CastTargetToDatumType(c.f.mem.LookupPrivate(colTyp).(coltypes.T))
	return c.f.mem.InternType(datumTyp)
}

// BoolType returns the private ID of the boolean SQL type.
func (c *CustomFuncs) BoolType() memo.PrivateID {
	return c.f.InternType(types.Bool)
}

// AnyType returns the private ID of the wildcard Any type.
func (c *CustomFuncs) AnyType() memo.PrivateID {
	return c.f.InternType(types.Any)
}

// CanConstructBinary returns true if (op left right) has a valid binary op
// overload and is therefore legal to construct. For example, while
// (Minus <date> <int>) is valid, (Minus <int> <date>) is not.
func (c *CustomFuncs) CanConstructBinary(op opt.Operator, left, right memo.GroupID) bool {
	leftType := c.LookupScalar(left).Type
	rightType := c.LookupScalar(right).Type
	return memo.BinaryOverloadExists(opt.MinusOp, rightType, leftType)
}

// ----------------------------------------------------------------------
//
// Property functions
//   General custom match and replace functions used to test expression
//   logical properties.
//
// ----------------------------------------------------------------------

// HasOuterCols returns true if the given group has at least one outer column,
// or in other words, a reference to a variable that is not bound within its
// own scope. For example:
//
//   SELECT * FROM a WHERE EXISTS(SELECT * FROM b WHERE b.x = a.x)
//
// The a.x variable in the EXISTS subquery references a column outside the scope
// of the subquery. It is an "outer column" for the subquery (see the comment on
// RelationalProps.OuterCols for more details).
func (c *CustomFuncs) HasOuterCols(group memo.GroupID) bool {
	return !c.OuterCols(group).Empty()
}

// OnlyConstants returns true if the scalar expression is a "constant
// expression tree", meaning that it will always evaluate to the same result.
// See the CommuteConst pattern comment for more details.
func (c *CustomFuncs) OnlyConstants(group memo.GroupID) bool {
	scalar := c.LookupScalar(group)
	return scalar.OuterCols.Empty() && !scalar.CanHaveSideEffects
}

// HasNoCols returns true if the group has zero output columns.
func (c *CustomFuncs) HasNoCols(group memo.GroupID) bool {
	return c.OutputCols(group).Empty()
}

// HasSameCols returns true if the two groups have an identical set of output
// columns.
func (c *CustomFuncs) HasSameCols(left, right memo.GroupID) bool {
	return c.OutputCols(left).Equals(c.OutputCols(right))
}

// HasSubsetCols returns true if the left group's output columns are a subset of
// the right group's output columns.
func (c *CustomFuncs) HasSubsetCols(left, right memo.GroupID) bool {
	return c.OutputCols(left).SubsetOf(c.OutputCols(right))
}

// HasZeroRows returns true if the given group never returns any rows.
func (c *CustomFuncs) HasZeroRows(group memo.GroupID) bool {
	return c.f.mem.GroupProperties(group).Relational.Cardinality.IsZero()
}

// HasOneRow returns true if the given group always returns exactly one row.
func (c *CustomFuncs) HasOneRow(group memo.GroupID) bool {
	return c.f.mem.GroupProperties(group).Relational.Cardinality.IsOne()
}

// HasZeroOrOneRow returns true if the given group returns at most one row.
func (c *CustomFuncs) HasZeroOrOneRow(group memo.GroupID) bool {
	return c.f.mem.GroupProperties(group).Relational.Cardinality.IsZeroOrOne()
}

// CanHaveZeroRows returns true if the given group might return zero rows.
func (c *CustomFuncs) CanHaveZeroRows(group memo.GroupID) bool {
	return c.f.mem.GroupProperties(group).Relational.Cardinality.CanBeZero()
}

// ----------------------------------------------------------------------
//
// Ordering functions
//   General custom match and replace functions related to orderings.
//
// ----------------------------------------------------------------------

// HasColsInOrdering returns true if all columns that appear in an ordering are
// output columns of the given group.
func (c *CustomFuncs) HasColsInOrdering(group memo.GroupID, ordering memo.PrivateID) bool {
	outCols := c.OutputCols(group)
	return c.ExtractOrdering(ordering).CanProjectCols(outCols)
}

// PruneOrdering removes any columns referenced by an OrderingChoice that are
// not output columns of the given group. Can only be called if
// HasColsInOrdering is true.
func (c *CustomFuncs) PruneOrdering(group memo.GroupID, ordering memo.PrivateID) memo.PrivateID {
	outCols := c.OutputCols(group)
	ord := c.ExtractOrdering(ordering)
	if ord.SubsetOfCols(outCols) {
		return ordering
	}
	ordCopy := ord.Copy()
	ordCopy.ProjectCols(outCols)
	return c.f.InternOrderingChoice(&ordCopy)
}

// ----------------------------------------------------------------------
//
// Projection construction functions
//   General helper functions to construct Projections.
//
// ----------------------------------------------------------------------

// ProjectColsFromBoth returns a Projections operator that combines distinct
// columns from both the provided left and right groups. If the group is a
// Projections operator, then the projected expressions will be directly added
// to the new Projections operator. Otherwise, the group's output columns will
// be added as passthrough columns to the new Projections operator.
func (c *CustomFuncs) ProjectColsFromBoth(left, right memo.GroupID) memo.GroupID {
	pb := projectionsBuilder{f: c.f}
	if c.Operator(left) == opt.ProjectionsOp {
		pb.addProjections(left)
	} else {
		pb.addPassthroughCols(c.OutputCols(left))
	}
	if c.Operator(right) == opt.ProjectionsOp {
		pb.addProjections(right)
	} else {
		pb.addPassthroughCols(c.OutputCols(right))
	}
	return pb.buildProjections()
}

// ----------------------------------------------------------------------
//
// Select Rules
//   Custom match and replace functions used with select.opt rules.
//
// ----------------------------------------------------------------------

// IsCorrelated returns true if any variable in the source expression references
// a column from the destination expression. For example:
//   (InnerJoin
//     (Scan a)
//     (Scan b)
//     (Eq (Variable a.x) (Const 1))
//   )
//
// The (Eq) expression is correlated with the (Scan a) expression because it
// references one of its columns. But the (Eq) expression is not correlated
// with the (Scan b) expression.
func (c *CustomFuncs) IsCorrelated(src, dst memo.GroupID) bool {
	return c.OuterCols(src).Intersects(c.OutputCols(dst))
}

// ConcatFilters creates a new Filters operator that contains conditions from
// both the left and right boolean filter expressions. If the left or right
// expression is itself a Filters operator, then it is "flattened" by merging
// its conditions into the new Filters operator.
func (c *CustomFuncs) ConcatFilters(left, right memo.GroupID) memo.GroupID {
	leftExpr := c.f.mem.NormExpr(left)
	rightExpr := c.f.mem.NormExpr(right)

	// Handle cases where left/right filters are constant boolean values.
	if leftExpr.Operator() == opt.TrueOp || rightExpr.Operator() == opt.FalseOp {
		return right
	}
	if rightExpr.Operator() == opt.TrueOp || leftExpr.Operator() == opt.FalseOp {
		return left
	}

	// Determine how large to make the conditions slice (at least 2 slots).
	cnt := 2
	leftFiltersExpr := leftExpr.AsFilters()
	if leftFiltersExpr != nil {
		cnt += int(leftFiltersExpr.Conditions().Length) - 1
	}
	rightFiltersExpr := rightExpr.AsFilters()
	if rightFiltersExpr != nil {
		cnt += int(rightFiltersExpr.Conditions().Length) - 1
	}

	// Create the conditions slice and populate it.
	lb := xfunc.MakeListBuilder(&c.CustomFuncs)

	if leftFiltersExpr != nil {
		lb.AddItems(c.f.mem.LookupList(leftFiltersExpr.Conditions()))
	} else {
		lb.AddItem(left)
	}
	if rightFiltersExpr != nil {
		lb.AddItems(c.f.mem.LookupList(rightFiltersExpr.Conditions()))
	} else {
		lb.AddItem(right)
	}
	return c.f.ConstructFilters(lb.BuildList())
}

// ----------------------------------------------------------------------
//
// GroupBy Rules
//   Custom match and replace functions used with groupby.opt rules.
//
// ----------------------------------------------------------------------

// GroupingAndConstCols returns the grouping columns and ConstAgg columns (for
// which the input and output column IDs match). A filter on these columns can
// be pushed through a GroupBy.
func (c *CustomFuncs) GroupingAndConstCols(def memo.PrivateID, aggs memo.GroupID) opt.ColSet {
	result := c.f.mem.LookupPrivate(def).(*memo.GroupByDef).GroupingCols.Copy()

	aggsExpr := c.f.mem.NormExpr(aggs).AsAggregations()
	aggsElems := c.f.mem.LookupList(aggsExpr.Aggs())
	aggsColList := c.ExtractColList(aggsExpr.Cols())

	// Add any ConstAgg columns.
	for i := range aggsElems {
		if constAgg := c.f.mem.NormExpr(aggsElems[i]).AsConstAgg(); constAgg != nil {
			// Verify that the input and output column IDs match.
			if aggsColList[i] == c.ExtractColID(c.f.mem.NormExpr(constAgg.Input()).AsVariable().Col()) {
				result.Add(int(aggsColList[i]))
			}
		}
	}
	return result
}

// GroupingColsAreKey returns true if the given group by's grouping columns form
// a strict key for the output rows of the given group. A strict key means that
// any two rows will have unique key column values. Nulls are treated as equal
// to one another (i.e. no duplicate nulls allowed). Having a strict key means
// that the set of key column values uniquely determine the values of all other
// columns in the relation.
func (c *CustomFuncs) GroupingColsAreKey(def memo.PrivateID, group memo.GroupID) bool {
	colSet := c.f.mem.LookupPrivate(def).(*memo.GroupByDef).GroupingCols
	return c.LookupLogical(group).Relational.FuncDeps.ColsAreStrictKey(colSet)
}

// IsUnorderedGroupBy returns true if the given input ordering for the group by
// is unspecified.
func (c *CustomFuncs) IsUnorderedGroupBy(def memo.PrivateID) bool {
	return c.f.mem.LookupPrivate(def).(*memo.GroupByDef).Ordering.Any()
}

// ----------------------------------------------------------------------
//
// Limit Rules
//   Custom match and replace functions used with limit.opt rules.
//
// ----------------------------------------------------------------------

// LimitGeMaxRows returns true if the given constant limit value is greater than
// or equal to the max number of rows returned by the input group.
func (c *CustomFuncs) LimitGeMaxRows(limit memo.PrivateID, input memo.GroupID) bool {
	limitVal := int64(*c.f.mem.LookupPrivate(limit).(*tree.DInt))
	maxRows := c.f.mem.GroupProperties(input).Relational.Cardinality.Max
	return limitVal >= 0 && maxRows < math.MaxUint32 && limitVal >= int64(maxRows)
}

// ----------------------------------------------------------------------
//
// Boolean Rules
//   Custom match and replace functions used with bool.opt rules.
//
// ----------------------------------------------------------------------

// SimplifyAnd removes True operands from an And operator, and eliminates the
// And operator altogether if any operand is False. It also "flattens" any And
// operator child by merging its conditions into the top-level list. Only one
// level of flattening is necessary, since this pattern would have already
// matched any And operator children. If, after simplification, no operands
// remain, then simplifyAnd returns True.
func (c *CustomFuncs) SimplifyAnd(conditions memo.ListID) memo.GroupID {
	lb := xfunc.MakeListBuilder(&c.CustomFuncs)
	for _, item := range c.f.mem.LookupList(conditions) {
		itemExpr := c.f.mem.NormExpr(item)

		switch itemExpr.Operator() {
		case opt.AndOp:
			// Flatten nested And operands.
			lb.AddItems(c.f.mem.LookupList(itemExpr.AsAnd().Conditions()))

		case opt.TrueOp:
			// And operator skips True operands.

		case opt.FalseOp:
			// Entire And evaluates to False if any operand is False.
			return item

		default:
			lb.AddItem(item)
		}
	}

	if lb.Empty() {
		return c.f.ConstructTrue()
	}
	return c.f.ConstructAnd(lb.BuildList())
}

// SimplifyOr removes False operands from an Or operator, and eliminates the Or
// operator altogether if any operand is True. It also "flattens" any Or
// operator child by merging its conditions into the top-level list. Only one
// level of flattening is necessary, since this pattern would have already
// matched any Or operator children. If, after simplification, no operands
// remain, then simplifyOr returns False.
func (c *CustomFuncs) SimplifyOr(conditions memo.ListID) memo.GroupID {
	lb := xfunc.MakeListBuilder(&c.CustomFuncs)

	for _, item := range c.f.mem.LookupList(conditions) {
		itemExpr := c.f.mem.NormExpr(item)

		switch itemExpr.Operator() {
		case opt.OrOp:
			// Flatten nested Or operands.
			lb.AddItems(c.f.mem.LookupList(itemExpr.AsOr().Conditions()))

		case opt.FalseOp:
			// Or operator skips False operands.

		case opt.TrueOp:
			// Entire Or evaluates to True if any operand is True.
			return item

		default:
			lb.AddItem(item)
		}
	}

	if lb.Empty() {
		return c.f.ConstructFalse()
	}
	return c.f.ConstructOr(lb.BuildList())
}

// SimplifyFilters behaves the same way as simplifyAnd, with one addition: if
// the conditions include a Null value in any position, then the entire
// expression is False. This works because the Filters expression only appears
// as a Select or Join filter condition, both of which treat a Null filter
// conjunct exactly as if it were False.
func (c *CustomFuncs) SimplifyFilters(conditions memo.ListID) memo.GroupID {
	lb := xfunc.MakeListBuilder(&c.CustomFuncs)
	for _, item := range c.f.mem.LookupList(conditions) {
		itemExpr := c.f.mem.NormExpr(item)

		switch itemExpr.Operator() {
		case opt.AndOp:
			// Flatten nested And operands.
			lb.AddItems(c.f.mem.LookupList(itemExpr.AsAnd().Conditions()))

		case opt.TrueOp:
			// Filters operator skips True operands.

		case opt.FalseOp:
			// Filters expression evaluates to False if any operand is False.
			return item

		case opt.NullOp:
			// Filters expression evaluates to False if any operand is False.
			return c.f.ConstructFalse()

		default:
			lb.AddItem(item)
		}
	}

	if lb.Empty() {
		return c.f.ConstructTrue()
	}
	return c.f.ConstructFilters(lb.BuildList())
}

// NegateConditions negates all the conditions in a list like:
//   a = 5 AND b < 10
// to:
//   NOT(a = 5) AND NOT(b < 10)
func (c *CustomFuncs) NegateConditions(conditions memo.ListID) memo.ListID {
	lb := xfunc.MakeListBuilder(&c.CustomFuncs)
	list := c.f.mem.LookupList(conditions)
	for i := range list {
		lb.AddItem(c.f.ConstructNot(list[i]))
	}
	return lb.BuildList()
}

// NegateComparison negates a comparison op like:
//   a.x = 5
// to:
//   a.x <> 5
func (c *CustomFuncs) NegateComparison(cmp opt.Operator, left, right memo.GroupID) memo.GroupID {
	negate := opt.NegateOpMap[cmp]
	operands := memo.DynamicOperands{memo.DynamicID(left), memo.DynamicID(right)}
	return c.f.DynamicConstruct(negate, operands)
}

// CommuteInequality swaps the operands of an inequality comparison expression,
// changing the operator to compensate:
//   5 < x
// to:
//   x > 5
func (c *CustomFuncs) CommuteInequality(op opt.Operator, left, right memo.GroupID) memo.GroupID {
	switch op {
	case opt.GeOp:
		return c.f.ConstructLe(right, left)
	case opt.GtOp:
		return c.f.ConstructLt(right, left)
	case opt.LeOp:
		return c.f.ConstructGe(right, left)
	case opt.LtOp:
		return c.f.ConstructGt(right, left)
	}
	panic(fmt.Sprintf("called commuteInequality with operator %s", op))
}

// IsRedundantSubclause checks if the given subclause appears as a conjunct in
// all of the given OR conditions. For example, if A is the given subclause:
//   A OR A                      =>  true
//   B OR A                      =>  false
//   A OR (A AND B)              =>  true
//   (A AND B) OR (A AND C)      =>  true
//   A OR (A AND B) OR (B AND C) =>  false
func (c *CustomFuncs) IsRedundantSubclause(conditions memo.ListID, subclause memo.GroupID) bool {
	for _, item := range c.f.mem.LookupList(conditions) {
		itemExpr := c.f.mem.NormExpr(item)

		switch itemExpr.Operator() {
		case opt.AndOp:
			found := false
			for _, item := range c.f.mem.LookupList(itemExpr.AsAnd().Conditions()) {
				if item == subclause {
					found = true
					break
				}
			}
			if !found {
				return false
			}

		default:
			if item != subclause {
				return false
			}
		}
	}

	return true
}

// ExtractRedundantSubclause extracts a redundant subclause from a list of OR
// conditions, and returns an AND of the subclause with the remaining OR
// expression (a logically equivalent expression). For example:
//   (A AND B) OR (A AND C)  =>  A AND (B OR C)
//
// If extracting the subclause from one of the OR conditions would result in an
// empty condition, the subclause itself is returned (a logically equivalent
// expression). For example:
//   A OR (A AND B)  =>  A
//
// These transformations are useful for finding a conjunct that can be pushed
// down in the query tree. For example, if the redundant subclause A is
// fully bound by one side of a join, it can be pushed through the join, even
// if B AND C cannot.
func (c *CustomFuncs) ExtractRedundantSubclause(
	conditions memo.ListID, subclause memo.GroupID,
) memo.GroupID {
	orList := xfunc.MakeListBuilder(&c.CustomFuncs)

	for _, item := range c.f.mem.LookupList(conditions) {
		itemExpr := c.f.mem.NormExpr(item)

		switch itemExpr.Operator() {
		case opt.AndOp:
			// Remove the subclause from the AND expression, and add the new AND
			// expression to the list of OR conditions.
			remaining := c.RemoveListItem(itemExpr.AsAnd().Conditions(), subclause)
			orList.AddItem(c.f.ConstructAnd(remaining))

		default:
			// In the default case, we have a boolean expression such as
			// `(A AND B) OR A`, which is logically equivalent to `A`.
			// (`A` is the redundant subclause).
			if item == subclause {
				return item
			}

			panic(fmt.Errorf(
				"ExtractRedundantSubclause called with non-redundant subclause:\n%s",
				memo.MakeNormExprView(c.f.mem, subclause).FormatString(opt.ExprFmtHideScalars),
			))
		}
	}

	or := c.f.ConstructOr(orList.BuildList())

	andList := xfunc.MakeListBuilder(&c.CustomFuncs)
	andList.AddItem(subclause)
	andList.AddItem(or)

	return c.f.ConstructAnd(andList.BuildList())
}

// ----------------------------------------------------------------------
//
// Comparison Rules
//   Custom match and replace functions used with comp.opt rules.
//
// ----------------------------------------------------------------------

// NormalizeTupleEquality remaps the elements of two tuples compared for
// equality, like this:
//   (a, b, c) = (x, y, z)
// into this:
//   (a = x) AND (b = y) AND (c = z)
func (c *CustomFuncs) NormalizeTupleEquality(left, right memo.ListID) memo.GroupID {
	if left.Length != right.Length {
		panic("tuple length mismatch")
	}

	lb := xfunc.MakeListBuilder(&c.CustomFuncs)
	leftList := c.f.mem.LookupList(left)
	rightList := c.f.mem.LookupList(right)
	for i := 0; i < len(leftList); i++ {
		lb.AddItem(c.f.ConstructEq(leftList[i], rightList[i]))
	}
	return c.f.ConstructAnd(lb.BuildList())
}

// ----------------------------------------------------------------------
//
// Scalar Rules
//   Custom match and replace functions used with scalar.opt rules.
//
// ----------------------------------------------------------------------

// SimplifyCoalesce discards any leading null operands, and then if the next
// operand is a constant, replaces with that constant.
func (c *CustomFuncs) SimplifyCoalesce(args memo.ListID) memo.GroupID {
	argList := c.f.mem.LookupList(args)
	for i := 0; i < int(args.Length-1); i++ {
		// If item is not a constant value, then its value may turn out to be
		// null, so no more folding. Return operands from then on.
		item := c.f.mem.NormExpr(argList[i])
		if !item.IsConstValue() {
			return c.f.ConstructCoalesce(c.f.InternList(argList[i:]))
		}

		if item.Operator() != opt.NullOp {
			return argList[i]
		}
	}

	// All operands up to the last were null (or the last is the only operand),
	// so return the last operand without the wrapping COALESCE function.
	return argList[args.Length-1]
}

// AllowNullArgs returns true if the binary operator with the given inputs
// allows one of those inputs to be null. If not, then the binary operator will
// simply be replaced by null.
func (c *CustomFuncs) AllowNullArgs(op opt.Operator, left, right memo.GroupID) bool {
	leftType := c.LookupScalar(left).Type
	rightType := c.LookupScalar(right).Type
	return memo.BinaryAllowsNullArgs(op, leftType, rightType)
}

// FoldNullUnary replaces the unary operator with a typed null value having the
// same type as the unary operator would have.
func (c *CustomFuncs) FoldNullUnary(op opt.Operator, input memo.GroupID) memo.GroupID {
	typ := c.LookupScalar(input).Type
	return c.f.ConstructNull(c.f.InternType(memo.InferUnaryType(op, typ)))
}

// FoldNullBinary replaces the binary operator with a typed null value having
// the same type as the binary operator would have.
func (c *CustomFuncs) FoldNullBinary(op opt.Operator, left, right memo.GroupID) memo.GroupID {
	leftType := c.LookupScalar(left).Type
	rightType := c.LookupScalar(right).Type
	return c.f.ConstructNull(c.f.InternType(memo.InferBinaryType(op, leftType, rightType)))
}

// ----------------------------------------------------------------------
//
// Numeric Rules
//   Custom match and replace functions used with numeric.opt rules.
//
// ----------------------------------------------------------------------

// EqualsNumber returns true if the given private numeric value (decimal, float,
// or integer) is equal to the given integer value.
func (c *CustomFuncs) EqualsNumber(private memo.PrivateID, value int64) bool {
	d := c.f.mem.LookupPrivate(private).(tree.Datum)
	switch t := d.(type) {
	case *tree.DDecimal:
		if value == 0 {
			return t.Decimal.IsZero()
		} else if value == 1 {
			return t.Decimal.Cmp(&tree.DecimalOne.Decimal) == 0
		}
		var dec apd.Decimal
		dec.SetInt64(value)
		return t.Decimal.Cmp(&dec) == 0

	case *tree.DFloat:
		return *t == tree.DFloat(value)

	case *tree.DInt:
		return *t == tree.DInt(value)
	}
	return false
}

// CanFoldUnaryMinus checks if a constant numeric value can be negated.
func (c *CustomFuncs) CanFoldUnaryMinus(input memo.GroupID) bool {
	d := c.f.mem.LookupPrivate(c.f.mem.NormExpr(input).AsConst().Value()).(tree.Datum)
	if t, ok := d.(*tree.DInt); ok {
		return *t != math.MinInt64
	}
	return true
}

// NegateNumeric applies a unary minus to a numeric value.
func (c *CustomFuncs) NegateNumeric(input memo.GroupID) memo.GroupID {
	ev := memo.MakeNormExprView(c.f.mem, input)
	r, err := memo.EvalUnaryOp(c.f.evalCtx, opt.UnaryMinusOp, ev)
	if err != nil {
		panic(err)
	}
	return c.f.ConstructConst(c.f.InternDatum(r))
}

// ExtractConstValue extracts the Datum from a constant value.
func (c *CustomFuncs) ExtractConstValue(group memo.GroupID) interface{} {
	return c.f.mem.LookupPrivate(c.f.mem.NormExpr(group).AsConst().Value())
}

// IsJSONScalar returns if the JSON value is a number, string, true, false, or null.
func (c *CustomFuncs) IsJSONScalar(value memo.GroupID) bool {
	v := c.ExtractConstValue(value).(*tree.DJSON)
	return v.JSON.Type() != json.ObjectJSONType && v.JSON.Type() != json.ArrayJSONType
}

// MakeSingleKeyJSONObject returns a JSON object with one entry, mapping key to value.
func (c *CustomFuncs) MakeSingleKeyJSONObject(key, value memo.GroupID) memo.GroupID {
	k := c.ExtractConstValue(key).(*tree.DString)
	v := c.ExtractConstValue(value).(*tree.DJSON)

	builder := json.NewObjectBuilder(1)
	builder.Add(string(*k), v.JSON)
	j := builder.Build()

	return c.f.ConstructConst(c.f.InternDatum(&tree.DJSON{JSON: j}))
}
