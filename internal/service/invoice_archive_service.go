package service

import (
	"context"
)

func (s *Service) ArchiveInvoiceDocument(ctx context.Context, p Principal, invoiceID, documentType, originalName, mediaType, objectID, checksum string) (string, error) {
	if _, err := required(objectID, "storage_object_id"); err != nil {
		return "", err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := archiveInvoiceDocumentTx(ctx, tx, p, invoiceID, documentType, originalName, mediaType, objectID, checksum)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}
