package goorderfulfillment

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// PackItems 把一张订单下若干分配的“已拣未发未装箱”商品装成一个待出库包裹。
//
//   - 装箱只移动分配的 PackedQty 计数，库存仍停留在 Picked 桶，包裹出库时才结转至 Shipped；
//   - 已装箱的数量不能再通过 Ship 直接发货，保证同一件商品不会被重复发出；
//   - 整批先校验后落账，任何一件数量不足即整批拒绝。
func (s *FulfillmentService) PackItems(req PackItemsRequest) (*PackageView, error) {
	if req.OrderID == "" || req.Destination == "" || req.Carrier == "" {
		return nil, fmt.Errorf("%w: order id, destination and carrier are required", ErrInvalidArgument)
	}
	if len(req.Items) == 0 {
		return nil, fmt.Errorf("%w: at least one pack item is required", ErrInvalidArgument)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	order := s.orders[req.OrderID]
	if order == nil {
		return nil, fmt.Errorf("%w: %s", ErrOrderNotFound, req.OrderID)
	}

	seen := make(map[string]bool)
	for _, item := range req.Items {
		if item.AllocationID == "" || item.Quantity <= 0 {
			return nil, fmt.Errorf("%w: each pack item needs an allocation id and positive quantity", ErrInvalidArgument)
		}
		if seen[item.AllocationID] {
			return nil, fmt.Errorf("%w: duplicate allocation %s in pack request", ErrInvalidArgument, item.AllocationID)
		}
		seen[item.AllocationID] = true
		alloc := s.allocations[item.AllocationID]
		if alloc == nil {
			return nil, fmt.Errorf("%w: %s", ErrAllocationNotFound, item.AllocationID)
		}
		if alloc.OrderID != order.ID {
			return nil, fmt.Errorf("%w: allocation %s belongs to order %s", ErrInvalidArgument, alloc.ID, alloc.OrderID)
		}
		if item.Quantity > alloc.PickedQty-alloc.ShippedQty-alloc.PackedQty {
			return nil, fmt.Errorf("%w: allocation %s picked=%d shipped=%d packed=%d requested=%d",
				ErrPackExceedsPicked, alloc.ID, alloc.PickedQty, alloc.ShippedQty, alloc.PackedQty, item.Quantity)
		}
	}

	now := s.now()
	items := make([]packageItem, 0, len(req.Items))
	for _, item := range req.Items {
		alloc := s.allocations[item.AllocationID]
		alloc.PackedQty += item.Quantity
		alloc.UpdatedAt = now
		items = append(items, packageItem{AllocationID: alloc.ID, SKU: alloc.LineSKU, Quantity: item.Quantity})
	}

	s.packageSeq++
	pkg := &Package{
		ID:          fmt.Sprintf("%s#pkg-%06d", order.ID, s.packageSeq),
		OrderID:     order.ID,
		Destination: req.Destination,
		Carrier:     req.Carrier,
		items:       items,
		Items:       aggregateItems(items),
		Status:      PackagePending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.packages[pkg.ID] = pkg
	order.UpdatedAt = now

	view := packageViewOf(pkg)
	return &view, nil
}

// MergePackages 包裹合并申请。
//
//   - 申请固定订单、包裹集合、商品明细与承运约束；集合中存在任何不可合并包裹
//     （不存在、不属于本订单、已出库、承运中、已合并、目的地或承运约束不一致）
//     时整单拒绝，不产生任何状态变化；
//   - 确认后只产生一个新包裹，原包裹保留并标记为 merged，历史不删除；
//   - 同一合并号相同内容重放返回原结果（Replayed=true）；包裹集合、目的地或
//     商品明细变化返回 ErrMergeConflict；承运约束变化返回 ErrMergeCarrierConflict。
func (s *FulfillmentService) MergePackages(req MergePackagesRequest) (*MergePackagesResult, error) {
	items, _, err := normalizeLines(req.Items)
	if err != nil {
		return nil, err
	}
	itemsKey := aggregateKey(items)
	if req.MergeID == "" || req.OrderID == "" || req.Destination == "" || req.Carrier == "" {
		return nil, fmt.Errorf("%w: merge id, order id, destination and carrier are required", ErrInvalidArgument)
	}
	if len(req.PackageIDs) < 2 {
		return nil, fmt.Errorf("%w: at least two packages are required to merge", ErrInvalidArgument)
	}
	seen := make(map[string]bool)
	sortedIDs := make([]string, 0, len(req.PackageIDs))
	for _, id := range req.PackageIDs {
		if id == "" {
			return nil, fmt.Errorf("%w: package id must not be empty", ErrInvalidArgument)
		}
		if seen[id] {
			return nil, fmt.Errorf("%w: duplicate package %s in merge request", ErrInvalidArgument, id)
		}
		seen[id] = true
		sortedIDs = append(sortedIDs, id)
	}
	sort.Strings(sortedIDs)
	fingerprint := mergeFingerprint(req.OrderID, sortedIDs, req.Destination, req.Carrier, itemsKey)

	s.mu.Lock()
	defer s.mu.Unlock()

	// 幂等重放：同一合并号已确认过。
	if rec := s.merges[req.MergeID]; rec != nil {
		if rec.fingerprint == fingerprint {
			return s.mergeResultLocked(rec, true), nil
		}
		// 仅承运约束不同：明确阻断原因；其余内容变化按通用冲突处理。
		if rec.OrderID == req.OrderID && rec.Destination == req.Destination &&
			equalStrings(rec.SourcePackageIDs, sortedIDs) && rec.Carrier != req.Carrier {
			return nil, fmt.Errorf("%w: merge %s confirmed with carrier %q, replay requested %q",
				ErrMergeCarrierConflict, req.MergeID, rec.Carrier, req.Carrier)
		}
		return nil, fmt.Errorf("%w: merge %s already confirmed with different packages, destination or items",
			ErrMergeConflict, req.MergeID)
	}

	order := s.orders[req.OrderID]
	if order == nil {
		return nil, fmt.Errorf("%w: %s", ErrOrderNotFound, req.OrderID)
	}
	if order.Status == StatusShipped {
		return nil, ErrAlreadyShipped
	}

	// 第一阶段：整集合校验，任何包裹不可合并即整单拒绝。
	sources := make([]*Package, 0, len(sortedIDs))
	for _, id := range sortedIDs {
		pkg := s.packages[id]
		if pkg == nil {
			return nil, fmt.Errorf("%w: %s", ErrPackageNotFound, id)
		}
		if pkg.OrderID != order.ID {
			return nil, fmt.Errorf("%w: package %s belongs to order %s", ErrPackageNotMergeable, id, pkg.OrderID)
		}
		if !pkg.Status.mergeable() {
			return nil, fmt.Errorf("%w: package %s is %s", ErrPackageNotMergeable, id, pkg.Status)
		}
		if pkg.Destination != req.Destination {
			return nil, fmt.Errorf("%w: package %s destination %q differs from %q",
				ErrPackageNotMergeable, id, pkg.Destination, req.Destination)
		}
		if pkg.Carrier != req.Carrier {
			return nil, fmt.Errorf("%w: package %s carrier %q differs from requested %q",
				ErrCarrierRestriction, id, pkg.Carrier, req.Carrier)
		}
		sources = append(sources, pkg)
	}

	// 商品明细必须与各原包裹合计完全一致。
	var mergedItems []packageItem
	for _, pkg := range sources {
		mergedItems = append(mergedItems, pkg.items...)
	}
	if aggregateKey(aggregateItems(mergedItems)) != itemsKey {
		return nil, fmt.Errorf("%w: merge %s declared items differ from source packages total",
			ErrMergeItemMismatch, req.MergeID)
	}

	// 第二阶段：落账。只产生一个新包裹；原包裹标记为 merged，记录保留。
	now := s.now()
	s.packageSeq++
	newPkg := &Package{
		ID:          fmt.Sprintf("%s#pkg-%06d", order.ID, s.packageSeq),
		OrderID:     order.ID,
		Destination: req.Destination,
		Carrier:     req.Carrier,
		items:       mergedItems,
		Items:       aggregateItems(mergedItems),
		Status:      PackagePending,
		MergeID:     req.MergeID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.packages[newPkg.ID] = newPkg
	for _, pkg := range sources {
		pkg.Status = PackageMerged
		pkg.MergedInto = newPkg.ID
		pkg.MergeID = req.MergeID
		pkg.UpdatedAt = now
	}

	s.mergeSeq++
	rec := &MergeRecord{
		MergeID:          req.MergeID,
		OrderID:          order.ID,
		SourcePackageIDs: sortedIDs,
		NewPackageID:     newPkg.ID,
		Destination:      req.Destination,
		Carrier:          req.Carrier,
		Items:            items,
		fingerprint:      fingerprint,
		seq:              s.mergeSeq,
		CreatedAt:        now,
	}
	s.merges[rec.MergeID] = rec
	order.UpdatedAt = now

	return s.mergeResultLocked(rec, false), nil
}

// mergeResultLocked 由合并记录构造结果（调用方持锁）。replayed 表示命中幂等。
func (s *FulfillmentService) mergeResultLocked(rec *MergeRecord, replayed bool) *MergePackagesResult {
	result := &MergePackagesResult{
		MergeID:    rec.MergeID,
		OrderID:    rec.OrderID,
		NewPackage: packageViewOf(s.packages[rec.NewPackageID]),
		Replayed:   replayed,
	}
	for _, id := range rec.SourcePackageIDs {
		result.MergedPackages = append(result.MergedPackages, packageViewOf(s.packages[id]))
	}
	return result
}

// PackageOutbound 包裹出库回执：pending → shipped。
// 按包裹明细逐条把来源分配的已拣数量结转为已发（Picked → Shipped），
// 并生成发货记录。已合并/已出库/承运中的包裹收到回执一律返回
// ErrStalePackage：旧包裹回执不能恢复已合并记录，也不会重复扣减库存。
func (s *FulfillmentService) PackageOutbound(req PackageOutboundRequest) (*PackageView, error) {
	if req.PackageID == "" {
		return nil, fmt.Errorf("%w: package id is required", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pkg := s.packages[req.PackageID]
	if pkg == nil {
		return nil, fmt.Errorf("%w: %s", ErrPackageNotFound, req.PackageID)
	}
	if pkg.Status != PackagePending {
		return nil, fmt.Errorf("%w: package %s is %s", ErrStalePackage, pkg.ID, pkg.Status)
	}
	order := s.orders[pkg.OrderID]

	now := s.now()
	for _, item := range pkg.items {
		alloc := s.allocations[item.AllocationID]
		alloc.PackedQty -= item.Quantity
		alloc.ShippedQty += item.Quantity
		alloc.UpdatedAt = now

		inv := s.inventories[invKey(alloc.WarehouseID, alloc.LineSKU)]
		inv.Picked -= item.Quantity
		inv.Shipped += item.Quantity

		s.shipmentSeq++
		s.shipments[order.ID] = append(s.shipments[order.ID], ShipmentRecord{
			ID:           fmt.Sprintf("%s#ship-%d", order.ID, s.shipmentSeq),
			OrderID:      order.ID,
			AllocationID: alloc.ID,
			LineSKU:      alloc.LineSKU,
			WarehouseID:  alloc.WarehouseID,
			Quantity:     item.Quantity,
			CreatedAt:    now,
		})
	}
	pkg.Status = PackageShipped
	pkg.UpdatedAt = now
	s.refreshStatus(order, now)
	order.UpdatedAt = now

	view := packageViewOf(pkg)
	return &view, nil
}

// PackageTransit 承运揽收回执：shipped → in_transit。
// 其他状态（未出库、已合并、已承运）收到回执一律返回 ErrStalePackage。
func (s *FulfillmentService) PackageTransit(req PackageTransitRequest) (*PackageView, error) {
	if req.PackageID == "" {
		return nil, fmt.Errorf("%w: package id is required", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pkg := s.packages[req.PackageID]
	if pkg == nil {
		return nil, fmt.Errorf("%w: %s", ErrPackageNotFound, req.PackageID)
	}
	if pkg.Status != PackageShipped {
		return nil, fmt.Errorf("%w: package %s is %s", ErrStalePackage, pkg.ID, pkg.Status)
	}
	now := s.now()
	pkg.Status = PackageInTransit
	pkg.UpdatedAt = now
	s.orders[pkg.OrderID].UpdatedAt = now

	view := packageViewOf(pkg)
	return &view, nil
}

// ---------- 纯函数辅助 ----------

// aggregateItems 把按来源分配的包裹明细聚合为按 SKU 升序的商品明细。
func aggregateItems(items []packageItem) []OrderLine {
	merged := make(map[string]int)
	skus := make([]string, 0, len(items))
	for _, item := range items {
		if _, ok := merged[item.SKU]; !ok {
			skus = append(skus, item.SKU)
		}
		merged[item.SKU] += item.Quantity
	}
	sort.Strings(skus)
	lines := make([]OrderLine, 0, len(skus))
	for _, sku := range skus {
		lines = append(lines, OrderLine{SKU: sku, Quantity: merged[sku]})
	}
	return lines
}

// aggregateKey 商品明细的规范化串（与 normalizeLines 的指纹输入格式一致）。
func aggregateKey(lines []OrderLine) string {
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "%s=%d;", l.SKU, l.Quantity)
	}
	return b.String()
}

// mergeFingerprint 合并申请内容的规范化指纹，用于幂等重放与冲突检测。
func mergeFingerprint(orderID string, packageIDs []string, destination, carrier, itemsKey string) string {
	raw := fmt.Sprintf("order=%s|packages=%s|dest=%s|carrier=%s|items=%s",
		orderID, strings.Join(packageIDs, ","), destination, carrier, itemsKey)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func packageViewOf(p *Package) PackageView {
	return PackageView{
		ID:          p.ID,
		OrderID:     p.OrderID,
		Destination: p.Destination,
		Carrier:     p.Carrier,
		Items:       append([]OrderLine(nil), p.Items...),
		Status:      p.Status,
		MergedInto:  p.MergedInto,
		MergeID:     p.MergeID,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	}
}

func mergeAuditOf(m *MergeRecord) MergeAudit {
	return MergeAudit{
		MergeID:          m.MergeID,
		OrderID:          m.OrderID,
		SourcePackageIDs: append([]string(nil), m.SourcePackageIDs...),
		NewPackageID:     m.NewPackageID,
		Destination:      m.Destination,
		Carrier:          m.Carrier,
		Items:            append([]OrderLine(nil), m.Items...),
		CreatedAt:        m.CreatedAt,
	}
}
