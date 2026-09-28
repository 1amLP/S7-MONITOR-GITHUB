#pragma once
// IMFActivate inherits IMFAttributes. Keep one real MF attribute store.
#define S7_ATTRIBUTES(store) \
HRESULT STDMETHODCALLTYPE GetItem(REFGUID k,PROPVARIANT* v) override{return store->GetItem(k,v);} \
HRESULT STDMETHODCALLTYPE GetItemType(REFGUID k,MF_ATTRIBUTE_TYPE* v) override{return store->GetItemType(k,v);} \
HRESULT STDMETHODCALLTYPE CompareItem(REFGUID k,REFPROPVARIANT v,BOOL* r) override{return store->CompareItem(k,v,r);} \
HRESULT STDMETHODCALLTYPE Compare(IMFAttributes* a,MF_ATTRIBUTES_MATCH_TYPE m,BOOL* r) override{return store->Compare(a,m,r);} \
HRESULT STDMETHODCALLTYPE GetUINT32(REFGUID k,UINT32* v) override{return store->GetUINT32(k,v);} \
HRESULT STDMETHODCALLTYPE GetUINT64(REFGUID k,UINT64* v) override{return store->GetUINT64(k,v);} \
HRESULT STDMETHODCALLTYPE GetDouble(REFGUID k,double* v) override{return store->GetDouble(k,v);} \
HRESULT STDMETHODCALLTYPE GetGUID(REFGUID k,GUID* v) override{return store->GetGUID(k,v);} \
HRESULT STDMETHODCALLTYPE GetStringLength(REFGUID k,UINT32* v) override{return store->GetStringLength(k,v);} \
HRESULT STDMETHODCALLTYPE GetString(REFGUID k,LPWSTR v,UINT32 n,UINT32* out) override{return store->GetString(k,v,n,out);} \
HRESULT STDMETHODCALLTYPE GetAllocatedString(REFGUID k,LPWSTR* v,UINT32* n) override{return store->GetAllocatedString(k,v,n);} \
HRESULT STDMETHODCALLTYPE GetBlobSize(REFGUID k,UINT32* n) override{return store->GetBlobSize(k,n);} \
HRESULT STDMETHODCALLTYPE GetBlob(REFGUID k,UINT8* v,UINT32 n,UINT32* out) override{return store->GetBlob(k,v,n,out);} \
HRESULT STDMETHODCALLTYPE GetAllocatedBlob(REFGUID k,UINT8** v,UINT32* n) override{return store->GetAllocatedBlob(k,v,n);} \
HRESULT STDMETHODCALLTYPE GetUnknown(REFGUID k,REFIID id,LPVOID* v) override{return store->GetUnknown(k,id,v);} \
HRESULT STDMETHODCALLTYPE SetItem(REFGUID k,REFPROPVARIANT v) override{return store->SetItem(k,v);} \
HRESULT STDMETHODCALLTYPE DeleteItem(REFGUID k) override{return store->DeleteItem(k);} \
HRESULT STDMETHODCALLTYPE DeleteAllItems() override{return store->DeleteAllItems();} \
HRESULT STDMETHODCALLTYPE SetUINT32(REFGUID k,UINT32 v) override{return store->SetUINT32(k,v);} \
HRESULT STDMETHODCALLTYPE SetUINT64(REFGUID k,UINT64 v) override{return store->SetUINT64(k,v);} \
HRESULT STDMETHODCALLTYPE SetDouble(REFGUID k,double v) override{return store->SetDouble(k,v);} \
HRESULT STDMETHODCALLTYPE SetGUID(REFGUID k,REFGUID v) override{return store->SetGUID(k,v);} \
HRESULT STDMETHODCALLTYPE SetString(REFGUID k,LPCWSTR v) override{return store->SetString(k,v);} \
HRESULT STDMETHODCALLTYPE SetBlob(REFGUID k,const UINT8* v,UINT32 n) override{return store->SetBlob(k,v,n);} \
HRESULT STDMETHODCALLTYPE SetUnknown(REFGUID k,IUnknown* v) override{return store->SetUnknown(k,v);} \
HRESULT STDMETHODCALLTYPE LockStore() override{return store->LockStore();} \
HRESULT STDMETHODCALLTYPE UnlockStore() override{return store->UnlockStore();} \
HRESULT STDMETHODCALLTYPE GetCount(UINT32* n) override{return store->GetCount(n);} \
HRESULT STDMETHODCALLTYPE GetItemByIndex(UINT32 i,GUID* k,PROPVARIANT* v) override{return store->GetItemByIndex(i,k,v);} \
HRESULT STDMETHODCALLTYPE CopyAllItems(IMFAttributes* d) override{return store->CopyAllItems(d);}
