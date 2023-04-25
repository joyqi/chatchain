import { HNSWLib } from "langchain/vectorstores/hnswlib";
import { getEmbeddings, Store } from ".";
import { Document } from "langchain/document";

export default class implements Store {
    private store: HNSWLib | undefined;

    async saveVector(docs: Document[]) {
        this.store = await HNSWLib.fromDocuments(docs, getEmbeddings());
    }

    getVectorStore() {
        if (!this.store) {
            throw new Error("Store not initialized");
        }

        return this.store.asRetriever();
    }
}