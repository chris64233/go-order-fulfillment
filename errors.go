package goorderfulfillment

import "errors"

// 哨兵错误：调用方可用 errors.Is 精确判定。
var (
	// ErrInsufficientInventory 订单确认或重配时全部仓库可用库存之和不足以完整满足。
	ErrInsufficientInventory = errors.New("insufficient inventory across warehouses")
	// ErrOrderConflict 相同外部订单号已确认，但商品或数量与本次不同。
	ErrOrderConflict = errors.New("order content conflict: same order id with different items or quantities")
	// ErrOrderNotFound 订单不存在。
	ErrOrderNotFound = errors.New("order not found")
	// ErrAllocationNotFound 指定的分配不存在。
	ErrAllocationNotFound = errors.New("allocation not found")
	// ErrStaleAllocation 迟到的拣货回执：目标分配已被关闭/取消/被新版方案取代。
	ErrStaleAllocation = errors.New("stale picking receipt: allocation already closed or superseded")
	// ErrReceiptOverflow 回执数量（成功+短缺）超过该分配尚未结清的数量。
	ErrReceiptOverflow = errors.New("picking receipt exceeds remaining allocation quantity")
	// ErrNothingToReallocate 没有需要重配的缺口（全部已由活跃分配覆盖或已拣齐/已取消）。
	ErrNothingToReallocate = errors.New("no uncovered gap to reallocate")
	// ErrShipExceedsPicked 发货数量超过该仓库已成功拣出且尚未发出的数量。
	ErrShipExceedsPicked = errors.New("shipment quantity exceeds picked quantity")
	// ErrAlreadyShipped 订单已全部发货，终态不可再操作。
	ErrAlreadyShipped = errors.New("order already fully shipped")
	// ErrInvalidArgument 入参非法（空订单号、非正数量、重复 SKU 等）。
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrPackageNotFound 指定的包裹不存在。
	ErrPackageNotFound = errors.New("package not found")
	// ErrPackageNotMergeable 包裹不可合并：已出库、承运中、已合并、不属于本订单或目的地不同。
	ErrPackageNotMergeable = errors.New("package not mergeable")
	// ErrStalePackage 迟到的包裹回执：包裹已合并/已出库/已承运，回执不能恢复或改变其状态。
	ErrStalePackage = errors.New("stale package receipt: package already merged, shipped or in transit")
	// ErrPackExceedsPicked 装箱数量超过该分配“已拣、未发、未装箱”的数量。
	ErrPackExceedsPicked = errors.New("pack quantity exceeds picked, unshipped and unpacked quantity")
	// ErrMergeConflict 相同合并号已确认，但包裹集合、目的地或商品明细与本次不同。
	ErrMergeConflict = errors.New("merge conflict: same merge id with different packages, destination or items")
	// ErrMergeCarrierConflict 相同合并号重放时承运约束发生变化，明确阻断并说明原因。
	ErrMergeCarrierConflict = errors.New("merge conflict: carrier constraint changed for the same merge id")
	// ErrCarrierRestriction 承运约束不满足：合并集合内包裹的承运约束与申请不一致。
	ErrCarrierRestriction = errors.New("carrier restriction violated")
	// ErrMergeItemMismatch 合并申请声明的商品明细与原包裹合计不一致。
	ErrMergeItemMismatch = errors.New("merge items mismatch: declared items differ from source packages")
)
