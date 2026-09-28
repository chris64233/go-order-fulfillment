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
)
