import { HNSWLib } from "langchain/vectorstores/hnswlib";
import { ConversationalRetrievalQAChain } from "langchain/chains";
import { getEmbeddings, getModel, Store } from ".";
import { Document } from "langchain/document";

export default class implements Store {
    private store: HNSWLib | undefined;

    async saveVector(docs: Document[]) {
        this.store = await HNSWLib.fromDocuments(docs, getEmbeddings());
    }

    async getChain() {
        if (!this.store) {
            throw new Error("Store not initialized");
        }

        return ConversationalRetrievalQAChain.fromLLM(getModel(), this.store.asRetriever());
    }
}