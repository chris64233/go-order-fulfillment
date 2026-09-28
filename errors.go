package goorderfulfillment

import "errors"

// 业务哨兵错误，调用方可使用 errors.Is 判定。
var (
	// ErrOrderNotFound 订单不存在。
	ErrOrderNotFound = errors.New("order not found")
	// ErrOrderAlreadyExists 订单已存在（内部不应直接返回，幂等路径返回原方案）。
	ErrOrderAlreadyExists = errors.New("order already exists")
	// ErrOrderConflict 同一外部订单号但商品或数量与首次确认不一致。
	ErrOrderConflict = errors.New("order conflict: content differs from original confirmation")
	// ErrInsufficientStock 全部仓库的可用库存不足以完整满足订单（或剩余缺口）。
	ErrInsufficientStock = errors.New("insufficient stock across warehouses")
	// ErrInvalidInput 入参非法（数量非正、空订单、重复条目等）。
	ErrInvalidInput = errors.New("invalid input")
	// ErrPlanNotFound 指定版本的方案不存在。
	ErrPlanNotFound = errors.New("allocation plan version not found")
	// ErrAllocationNotFound 方案中找不到指定“仓-SKU”的分配行。
	ErrAllocationNotFound = errors.New("allocation not found in plan")
	// ErrStaleReceipt 迟到的旧方案/已关闭分配行回执，不能再影响当前分配。
	ErrStaleReceipt = errors.New("stale picking receipt: allocation already closed")
	// ErrReceiptDuplicate 相同 ReceiptID 的回执重复提交。
	ErrReceiptDuplicate = errors.New("duplicate picking receipt")
	// ErrOverPicked 回执拣出数量超过该分配行的需求。
	ErrOverPicked = errors.New("picked quantity exceeds allocation")
	// ErrShipmentDuplicate 相同 ShipmentID 的发货重复提交。
	ErrShipmentDuplicate = errors.New("duplicate shipment")
	// ErrOverShipped 发货数量超过该仓已拣未发数量，或发货总量超过订单需求。
	ErrOverShipped = errors.New("shipment quantity exceeds picked quantity")
	// ErrOrderTerminal 订单已取消或已完成，不允许该操作。
	ErrOrderTerminal = errors.New("order already terminal")
	// ErrNothingToReallocate 当前不存在需要重配的缺口。
	ErrNothingToReallocate = errors.New("no shortage gap to reallocate")
)
